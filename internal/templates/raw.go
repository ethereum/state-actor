package templates

import (
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func init() {
	Register(&rawTemplate{})
}

// rawTemplate handles `kind: contract` with `code:` set. Emits one
// PreAllocEntity with the supplied bytecode verbatim plus synthesized
// storage slots filling approximate_size_bytes (if > 0).
type rawTemplate struct{}

// TemplateNameRaw is the registry key for this template; Name() returns
// this constant so cross-package consumers (specbuild's pickTemplate)
// can never drift from the registered name.
const TemplateNameRaw = "raw"

func (rawTemplate) Name() string      { return TemplateNameRaw }
func (rawTemplate) UserVisible() bool { return false }

func (rawTemplate) HonoredEntityFields() EntityFieldSet { return AllEntityFieldsHonored() }

func (rawTemplate) ValidateParameters(params map[string]any) error {
	if len(params) > 0 {
		return fmt.Errorf("raw template does not accept parameters (got %d keys)", len(params))
	}
	return nil
}

func (rawTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	if len(e.Code) == 0 {
		return nil, fmt.Errorf("raw template requires `code:` to be set on kind=contract")
	}

	balance := uint256.NewInt(0)
	if e.Balance != nil {
		balance = e.Balance.V
	}

	codeHash := crypto.Keccak256Hash(e.Code)

	acc := &types.StateAccount{
		Nonce:    e.Nonce,
		Balance:  balance,
		Root:     types.EmptyRootHash,
		CodeHash: codeHash.Bytes(),
	}

	pe := PreAllocEntity{
		Address: ctx.ResolvedAddress,
		Account: acc,
		Code:    e.Code,
	}

	if e.ApproximateSizeBytes > 0 {
		slotCount := ctx.Sizer.SlotsForBytes(ctx.ClientName, e.ApproximateSizeBytes)
		if slotCount > 0 {
			pe.Storage = SynthesizeSlots(ctx.Seed, ctx.ResolvedAddress, "raw", slotCount)
		}
	}

	return []PreAllocEntity{pe}, nil
}
