package templates

import (
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func init() {
	Register(&createPreimageDeploysTemplate{})
}

// createPreimageDeploysTemplate handles
// `kind: contract, template: create_preimage_deploys`. For each nonce
// in [start_nonce, start_nonce+count) it derives the CREATE address
// (keccak256(rlp([sender, nonce]))[12:] via crypto.CreateAddress) and
// plants `runtime` there with nonce=1, balance=0, no storage.
//
// The CREATE deployer is supplied via the required `sender:` parameter
// — NOT the entity's own resolved address. That keeps the user free to
// also declare a separate entity (e.g. `template: raw`) at the
// sender's address with that account's actual bytecode; `spec.Validate`
// only flags duplicate `address:` fields between entities, so giving
// the sender its own slot avoids that collision.
//
// Backs CreatePreimageLayout in execution-specs bloatnet benchmarks:
// the test code computes keccak256(rlp([sender, nonce]))[12:] at run
// time; this template pre-plants matching state at every such address
// so a single shared `runtime` (the case for Bittrex Controller's
// 1.5M children — all share the same body) becomes the prestate.
//
// `runtime` must be supplied verbatim (no EVM simulation). For
// initcodes whose deployed bytecode is sender/nonce-dependent (rare),
// the user must precompute the per-address code and use multiple
// entities instead of one big batch.
type createPreimageDeploysTemplate struct{}

func (createPreimageDeploysTemplate) Name() string      { return "create_preimage_deploys" }
func (createPreimageDeploysTemplate) UserVisible() bool { return true }

const createPreimageNonceLimit = uint64(1) << 32

func (createPreimageDeploysTemplate) ValidateParameters(params map[string]any) error {
	if err := RejectUnknownKeys(params, "create_preimage_deploys", []string{
		"sender", "start_nonce", "count", "runtime",
	}); err != nil {
		return err
	}
	for _, required := range []string{"sender", "count", "runtime"} {
		if _, ok := params[required]; !ok {
			return fmt.Errorf("create_preimage_deploys: missing required parameter %q", required)
		}
	}
	if _, err := ParseAddressParam(params["sender"], "sender"); err != nil {
		return fmt.Errorf("create_preimage_deploys: %w", err)
	}
	count, err := ParseUint64Param(params["count"], "count")
	if err != nil {
		return fmt.Errorf("create_preimage_deploys: %w", err)
	}
	if count > createPreimageNonceLimit {
		return fmt.Errorf("create_preimage_deploys: count=%d exceeds practical limit (2^32)", count)
	}
	runtime, err := ParseHexBytesParam(params["runtime"], "runtime")
	if err != nil {
		return fmt.Errorf("create_preimage_deploys: %w", err)
	}
	if len(runtime) == 0 {
		return fmt.Errorf("create_preimage_deploys: runtime must be non-empty")
	}
	if v, has := params["start_nonce"]; has {
		if _, err := ParseUint64Param(v, "start_nonce"); err != nil {
			return fmt.Errorf("create_preimage_deploys: %w", err)
		}
	}
	return nil
}

func (createPreimageDeploysTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	sender, _ := ParseAddressParam(e.Parameters["sender"], "sender")
	count, _ := ParseUint64Param(e.Parameters["count"], "count")
	runtime, _ := ParseHexBytesParam(e.Parameters["runtime"], "runtime")
	startNonce := uint64(0)
	if v, has := e.Parameters["start_nonce"]; has {
		startNonce, _ = ParseUint64Param(v, "start_nonce")
	}
	if count == 0 {
		return nil, nil
	}
	codeHash := crypto.Keccak256Hash(runtime).Bytes()
	out := make([]PreAllocEntity, 0, count)
	for i := uint64(0); i < count; i++ {
		derived := crypto.CreateAddress(sender, startNonce+i)
		out = append(out, PreAllocEntity{
			Address: derived,
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: codeHash,
			},
			Code: runtime,
		})
	}
	return out, nil
}
