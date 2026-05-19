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
var CanonicalCREATE2FactoryAddress = common.HexToAddress("0x4e59b44847b379578588920cA78FbF26c0B4956C")

// CanonicalCREATE2FactoryCode is the 69-byte runtime of the Arachnid
// factory at CanonicalCREATE2FactoryAddress (per Etherscan source).
// Reads salt(32) ++ initcode from calldata, performs CREATE2, returns
// the deployed address.
var CanonicalCREATE2FactoryCode = common.FromHex(
	"0x7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3",
)

// create2FactoryTemplate handles `kind: contract, template: create2_factory`.
// Plants the Arachnid factory runtime at the entity's resolved address
// (which must equal CanonicalCREATE2FactoryAddress).
//
// Accepts no parameters. Pair with `create2_deploys` when the spec needs
// both the factory and contracts deployed through it.
type create2FactoryTemplate struct{}

func (create2FactoryTemplate) Name() string      { return "create2_factory" }
func (create2FactoryTemplate) UserVisible() bool { return true }

func (create2FactoryTemplate) ValidateParameters(params map[string]any) error {
	if len(params) > 0 {
		return fmt.Errorf("create2_factory: does not accept parameters (got %d keys)", len(params))
	}
	return nil
}

func (create2FactoryTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	if ctx.ResolvedAddress != CanonicalCREATE2FactoryAddress {
		return nil, fmt.Errorf("create2_factory: resolved address %s does not match the canonical Arachnid factory %s; set `address: %s` on the entity",
			ctx.ResolvedAddress.Hex(),
			CanonicalCREATE2FactoryAddress.Hex(),
			CanonicalCREATE2FactoryAddress.Hex())
	}
	code := CanonicalCREATE2FactoryCode
	pe := PreAllocEntity{
		Address: ctx.ResolvedAddress,
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
