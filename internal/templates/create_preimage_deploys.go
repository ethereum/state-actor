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
// plants the derived contract's code there with nonce=1, balance=0,
// and the optional `storage_init` storage.
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
// Two mutually exclusive code modes (ValidateParameters enforces the
// mutex): literal mode supplies `runtime` verbatim (no EVM simulation;
// shared by every derived contract), and pattern mode names a built-in
// `code_pattern:` generator producing a per-derived-address-unique
// runtime (see code_pattern.go).
type createPreimageDeploysTemplate struct{}

// TemplateNameCreatePreimageDeploys is the registry key for this template.
const TemplateNameCreatePreimageDeploys = "create_preimage_deploys"

func (createPreimageDeploysTemplate) Name() string      { return TemplateNameCreatePreimageDeploys }
func (createPreimageDeploysTemplate) UserVisible() bool { return true }

const createPreimageNonceLimit = uint64(1) << 32

func (createPreimageDeploysTemplate) ValidateParameters(params map[string]any) error {
	if err := RejectUnknownKeys(params, "create_preimage_deploys", []string{
		"sender", "start_nonce", "count", "runtime", "storage_init", "code_pattern",
	}); err != nil {
		return err
	}
	for _, required := range []string{"sender", "count"} {
		if _, ok := params[required]; !ok {
			return fmt.Errorf("create_preimage_deploys: missing required parameter %q", required)
		}
	}
	// `code_pattern` opts into a named per-address runtime generator.
	// When set, `runtime` is forbidden. When unset, `runtime` is
	// required and supplied verbatim.
	if v, has := params["code_pattern"]; has {
		name, ok := v.(string)
		if !ok {
			return fmt.Errorf("create_preimage_deploys: code_pattern must be a string (got %T)", v)
		}
		if !IsKnownCodePattern(name) {
			return fmt.Errorf("create_preimage_deploys: unknown code_pattern %q (known: %q)",
				name, CodePatternUniqueJumpdestPreAmsterdam)
		}
		if _, has := params["runtime"]; has {
			return fmt.Errorf("create_preimage_deploys: `runtime` is forbidden when `code_pattern` is set (the pattern generates per-address runtime)")
		}
	} else {
		if _, ok := params["runtime"]; !ok {
			return fmt.Errorf("create_preimage_deploys: missing required parameter %q (or set `code_pattern:`)", "runtime")
		}
		runtime, err := ParseHexBytesParam(params["runtime"], "runtime")
		if err != nil {
			return fmt.Errorf("create_preimage_deploys: %w", err)
		}
		if len(runtime) == 0 {
			return fmt.Errorf("create_preimage_deploys: runtime must be non-empty")
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
	if v, has := params["start_nonce"]; has {
		if _, err := ParseUint64Param(v, "start_nonce"); err != nil {
			return fmt.Errorf("create_preimage_deploys: %w", err)
		}
	}
	if v, has := params["storage_init"]; has {
		if _, err := ParseStorageInitMap(v); err != nil {
			return fmt.Errorf("create_preimage_deploys: %w", err)
		}
	}
	return nil
}

func (createPreimageDeploysTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	sender, _ := ParseAddressParam(e.Parameters["sender"], "sender")
	count, _ := ParseUint64Param(e.Parameters["count"], "count")
	startNonce := uint64(0)
	if v, has := e.Parameters["start_nonce"]; has {
		startNonce, _ = ParseUint64Param(v, "start_nonce")
	}
	storageInit, err := ParseStorageInitMap(e.Parameters["storage_init"])
	if err != nil {
		return nil, fmt.Errorf("create_preimage_deploys: %w", err)
	}

	// Resolve per-address runtime: either `code_pattern` (per-address
	// unique) or the literal `runtime:` parameter (shared). Mutex
	// enforced in ValidateParameters.
	var patternName string
	var sharedRuntime []byte
	if v, has := e.Parameters["code_pattern"]; has {
		patternName, _ = v.(string)
	} else {
		sharedRuntime, _ = ParseHexBytesParam(e.Parameters["runtime"], "runtime")
	}

	if count == 0 {
		return nil, nil
	}
	out := make([]PreAllocEntity, 0, count)
	for i := uint64(0); i < count; i++ {
		derived := crypto.CreateAddress(sender, startNonce+i)
		runtime := sharedRuntime
		if patternName != "" {
			runtime, err = codePatternRuntimeFor(patternName, derived)
			if err != nil {
				return nil, fmt.Errorf("create_preimage_deploys: nonce=%d: %w", startNonce+i, err)
			}
		}
		out = append(out, PreAllocEntity{
			Address: derived,
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: crypto.Keccak256Hash(runtime).Bytes(),
			},
			Code:    runtime,
			Storage: MapToSeq(storageInit),
		})
	}
	return out, nil
}
