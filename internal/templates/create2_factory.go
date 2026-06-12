package templates

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func init() {
	Register(&create2FactoryTemplate{})
}

// CanonicalCREATE2FactoryAddress is the Arachnid deterministic-deployment
// proxy. The same bytes live on every EVM chain because the deploy tx is
// pre-signed; planting the runtime here lets us mirror that environment
// inside a state-actor genesis.
//
// Treat as immutable — exported as a package var only because Go has no
// const composite values. Mutating it (or CanonicalCREATE2FactoryCode)
// breaks this template, the Arachnid pairing check in internal/specbuild,
// and create2_deploys address derivation.
var CanonicalCREATE2FactoryAddress = common.HexToAddress("0x4e59b44847b379578588920cA78FbF26c0B4956C")

// CanonicalCREATE2FactoryCode is the 69-byte runtime of the Arachnid
// factory at CanonicalCREATE2FactoryAddress (per Etherscan source).
// Reads salt(32) ++ initcode from calldata, performs CREATE2, returns
// the deployed address.
//
// Treat as immutable (see CanonicalCREATE2FactoryAddress); the backing
// array is aliased into every expansion. Its keccak256 is pinned by
// TestCanonicalCREATE2FactoryCodeKeccakPin.
var CanonicalCREATE2FactoryCode = common.FromHex(
	"0x7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3",
)

// create2FactoryTemplate handles `kind: contract, template: create2_factory`.
// Plants the canonical Arachnid factory runtime at the entity's resolved
// address. When neither `address:` nor `name:` is set on the entity,
// the template defaults the planted address to the canonical Arachnid
// factory address (CanonicalCREATE2FactoryAddress) — picking the
// position-derived address ResolveAddress would otherwise return is
// almost certainly a user mistake for a singleton like this. Explicit
// `address:` (or `name:`-derived) values are honored verbatim; the
// runtime stays the canonical Arachnid bytecode regardless.
//
// Pair with `create2_deploys` when the spec needs both the factory and
// contracts deployed through it. If a `create2_deploys` entry uses the
// Arachnid factory (its `factory:` is unset or explicitly set to
// CanonicalCREATE2FactoryAddress), specbuild.Build enforces that a
// matching `create2_factory` entity exists at the Arachnid address.
type create2FactoryTemplate struct{}

// TemplateNameCreate2Factory is the registry key for this template.
// specbuild's Arachnid-pairing enforcement matches entities against it;
// Name() returns this constant so the two can never drift.
const TemplateNameCreate2Factory = "create2_factory"

func (create2FactoryTemplate) Name() string      { return TemplateNameCreate2Factory }
func (create2FactoryTemplate) UserVisible() bool { return true }

// HonoredEntityFields: the factory is a fixed singleton (canonical
// runtime, nonce 1, balance 0); only `address:`/`name:` anchoring —
// which is universal and not represented in the set — has any effect.
func (create2FactoryTemplate) HonoredEntityFields() EntityFieldSet { return EntityFieldSet{} }

func (create2FactoryTemplate) ValidateParameters(params map[string]any) error {
	if len(params) > 0 {
		return fmt.Errorf("create2_factory: does not accept parameters (got %d keys)", len(params))
	}
	return nil
}

// EffectiveCreate2FactoryAddress returns the address a create2_factory
// entity plants the canonical runtime at: the canonical Arachnid address
// when the user set neither `address:` nor `name:` (a seed-position-
// derived address is meaningless for a singleton factory), otherwise
// resolvedAddr — the entity address specbuild resolved.
//
// Single source of truth for the defaulting rule, shared by
// create2FactoryTemplate.Expand (resolvedAddr = ctx.ResolvedAddress) and
// specbuild's enforceArachnidFactoryRequirement (resolvedAddr =
// ResolveAddress(seed, e, i)).
func EffectiveCreate2FactoryAddress(e spec.Entity, resolvedAddr common.Address) common.Address {
	if e.Address == nil && e.Name == "" {
		return CanonicalCREATE2FactoryAddress
	}
	return resolvedAddr
}

func (create2FactoryTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	addr := EffectiveCreate2FactoryAddress(e, ctx.ResolvedAddress)
	code := CanonicalCREATE2FactoryCode
	pe := PreAllocEntity{
		Address: addr,
		Account: &types.StateAccount{
			Nonce:    1,
			Balance:  uint256.NewInt(0),
			Root:     types.EmptyRootHash,
			CodeHash: crypto.Keccak256Hash(code).Bytes(),
		},
		Code: code,
	}
	return []PreAllocEntity{pe}, nil
}
