package templates

import (
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func init() {
	Register(&eoaTemplate{})
}

// eoaTemplate handles `kind: eoa`. EOAs may carry EIP-7702 delegation
// code (23-byte 0xef0100<addr> marker) and custom storage.
type eoaTemplate struct{}

// TemplateNameEOA is the registry key for this template; Name() returns
// this constant so cross-package consumers (specbuild's pickTemplate)
// can never drift from the registered name.
const TemplateNameEOA = "eoa"

func (eoaTemplate) Name() string      { return TemplateNameEOA }
func (eoaTemplate) UserVisible() bool { return false }

func (eoaTemplate) HonoredEntityFields() EntityFieldSet { return AllEntityFieldsHonored() }

func (eoaTemplate) ValidateParameters(params map[string]any) error {
	if len(params) > 0 {
		return errEOAParameters
	}
	return nil
}

func (eoaTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	balance := uint256.NewInt(0)
	if e.Balance != nil {
		balance = e.Balance.V
	}

	acc := &types.StateAccount{
		Nonce:    e.Nonce,
		Balance:  balance,
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash[:],
	}

	if len(e.Code) > 0 {
		// Usually a 7702 delegation marker; arbitrary code is also accepted.
		acc.CodeHash = crypto.Keccak256Hash(e.Code).Bytes()
	}

	pe := PreAllocEntity{
		Address: ctx.ResolvedAddress,
		Account: acc,
		Code:    e.Code,
	}

	if e.ApproximateSizeBytes > 0 {
		slotCount := ctx.Sizer.SlotsForBytes(ctx.ClientName, e.ApproximateSizeBytes)
		if slotCount > 0 {
			pe.Storage = SynthesizeSlots(ctx.Seed, ctx.ResolvedAddress, "eoa", slotCount)
		}
	}

	return []PreAllocEntity{pe}, nil
}

var errEOAParameters = errString("eoa template does not accept parameters")

type errString string

func (e errString) Error() string { return string(e) }
