package templates

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func init() {
	Register(&create2DeploysTemplate{})
}

// create2DeploysTemplate handles `kind: contract, template: create2_deploys`.
// For each salt in [salt_start, salt_start+salt_count), derives a
// CREATE2 address (keccak256(0xff ++ factory ++ salt ++ keccak256(initcode))[12:])
// and plants `runtime` there with nonce=1, optionally with the
// per-derived-address storage prefilled from `storage_init`.
//
// The entity's resolved address is NOT used as an emission address —
// the template only emits at the derived addresses. The entity can
// still be named or anchored to a deterministic anchor address via
// `name:` / `address:` if the user wants reproducible spec ordering.
//
// `initcode` is required only to derive the CREATE2 address; the
// constructor is never executed. `runtime` is mandatory and supplied
// verbatim — the template does not run the EVM. To populate the
// derived address with the code an ADDRESS-dependent constructor would
// have emitted, the user must precompute the per-address runtime and
// declare multiple entities instead of one batch (or use the
// `create_preimage_deploys` template if CREATE-style derivation
// suffices).
//
// Symmetric with `create_preimage_deploys`: the only difference between
// the two templates is the address-derivation algorithm; all other
// account properties (`runtime`, `storage_init`, etc.) behave
// identically.
type create2DeploysTemplate struct{}

func (create2DeploysTemplate) Name() string      { return "create2_deploys" }
func (create2DeploysTemplate) UserVisible() bool { return true }

const create2DeploysSaltLimit = uint64(1) << 32

func (create2DeploysTemplate) ValidateParameters(params map[string]any) error {
	if err := RejectUnknownKeys(params, "create2_deploys", []string{
		"initcode", "salt_start", "salt_count", "runtime", "factory", "storage_init",
	}); err != nil {
		return err
	}
	for _, required := range []string{"initcode", "salt_count", "runtime"} {
		if _, ok := params[required]; !ok {
			return fmt.Errorf("create2_deploys: missing required parameter %q", required)
		}
	}
	initcode, err := ParseHexBytesParam(params["initcode"], "initcode")
	if err != nil {
		return fmt.Errorf("create2_deploys: %w", err)
	}
	if len(initcode) == 0 {
		return fmt.Errorf("create2_deploys: initcode must be non-empty")
	}
	runtime, err := ParseHexBytesParam(params["runtime"], "runtime")
	if err != nil {
		return fmt.Errorf("create2_deploys: %w", err)
	}
	if len(runtime) == 0 {
		return fmt.Errorf("create2_deploys: runtime must be non-empty (this template does not run the EVM; supply the runtime bytecode directly)")
	}
	saltCount, err := ParseUint64Param(params["salt_count"], "salt_count")
	if err != nil {
		return fmt.Errorf("create2_deploys: %w", err)
	}
	if saltCount > create2DeploysSaltLimit {
		return fmt.Errorf("create2_deploys: salt_count=%d exceeds practical limit (2^32)", saltCount)
	}
	if v, has := params["salt_start"]; has {
		if _, err := ParseUint64Param(v, "salt_start"); err != nil {
			return fmt.Errorf("create2_deploys: %w", err)
		}
	}
	if v, has := params["factory"]; has {
		if _, err := ParseAddressParam(v, "factory"); err != nil {
			return fmt.Errorf("create2_deploys: %w", err)
		}
	}
	if v, has := params["storage_init"]; has {
		if _, err := ParseStorageInitMap(v); err != nil {
			return fmt.Errorf("create2_deploys: %w", err)
		}
	}
	return nil
}

func (create2DeploysTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	initcode, _ := ParseHexBytesParam(e.Parameters["initcode"], "initcode")
	runtime, _ := ParseHexBytesParam(e.Parameters["runtime"], "runtime")
	saltCount, _ := ParseUint64Param(e.Parameters["salt_count"], "salt_count")

	saltStart := uint64(0)
	if v, has := e.Parameters["salt_start"]; has {
		saltStart, _ = ParseUint64Param(v, "salt_start")
	}

	factory := CanonicalCREATE2FactoryAddress
	if v, has := e.Parameters["factory"]; has {
		factory, _ = ParseAddressParam(v, "factory")
	}

	storageInit, err := ParseStorageInitMap(e.Parameters["storage_init"])
	if err != nil {
		return nil, fmt.Errorf("create2_deploys: %w", err)
	}

	if saltCount == 0 {
		return nil, nil
	}

	initHash := crypto.Keccak256(initcode)
	codeHash := crypto.Keccak256Hash(runtime).Bytes()
	out := make([]PreAllocEntity, 0, saltCount)
	for k := uint64(0); k < saltCount; k++ {
		var salt [32]byte
		binary.BigEndian.PutUint64(salt[24:], saltStart+k)
		derived := crypto.CreateAddress2(factory, salt, initHash)
		out = append(out, PreAllocEntity{
			Address: derived,
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: codeHash,
			},
			Code:    runtime,
			Storage: MapToSeq(storageInit),
		})
	}
	return out, nil
}
