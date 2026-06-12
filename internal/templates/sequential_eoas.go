package templates

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func init() {
	Register(&sequentialEOAsTemplate{})
}

// sequentialEOAsTemplate handles `kind: contract, template: sequential_eoas`.
// One entity expands into `count` plain EOAs at addresses
// [start, start+count), each funded with `balance`. Anchor address is
// the entity's resolved address (typically set explicitly via `address:`).
//
// `balance` defaults to 1 wei if omitted and MUST be non-zero — a
// zero-balance plain EOA (no code, nonce 0) would be pruned by
// EIP-161, leaving no account at the planted addresses for the
// benchmark to reference.
//
// Backs SequentialAddressLayout in execution-specs bloatnet benchmarks:
// the test code iterates address = start + i for i in 0..N; this template
// pre-plants matching state at every such address.
type sequentialEOAsTemplate struct{}

// TemplateNameSequentialEOAs is the registry key for this template.
const TemplateNameSequentialEOAs = "sequential_eoas"

func (sequentialEOAsTemplate) Name() string      { return TemplateNameSequentialEOAs }
func (sequentialEOAsTemplate) UserVisible() bool { return true }

func (sequentialEOAsTemplate) HonoredEntityFields() EntityFieldSet {
	return EntityFieldSet{
		Balance:              EntityFieldSupport{Alternative: "parameters.balance"},
		Nonce:                EntityFieldSupport{}, // derived EOAs are always nonce 0
		Code:                 EntityFieldSupport{}, // plain EOAs carry no code
		ApproximateSizeBytes: EntityFieldSupport{Alternative: "parameters.count"},
	}
}

// sequentialEOAsParams is the typed result of parseSequentialEOAsParams.
type sequentialEOAsParams struct {
	count   uint64       // in [1, 2^32]
	balance *uint256.Int // never nil, never zero; defaults to 1 wei
}

// parseSequentialEOAsParams is the single validation+parse boundary for
// this template. ValidateParameters and Expand both call it, so a
// parameter set that validates is — by construction — the same one that
// expands; no check can drift between the two entry points.
func parseSequentialEOAsParams(params map[string]any) (sequentialEOAsParams, error) {
	var pp sequentialEOAsParams
	if err := RejectUnknownKeys(params, "sequential_eoas", []string{"count", "balance"}); err != nil {
		return pp, err
	}
	if _, ok := params["count"]; !ok {
		return pp, fmt.Errorf("sequential_eoas: missing required parameter `count`")
	}
	count, err := ParseUint64Param(params["count"], "count")
	if err != nil {
		return pp, fmt.Errorf("sequential_eoas: %w", err)
	}
	if count == 0 {
		return pp, fmt.Errorf("sequential_eoas: count must be >= 1 (count=0 emits nothing; delete the entity instead)")
	}
	// Practical ceiling: 2^32 entries is already ~1 TB of accounts at
	// ~218 B/account on disk; anything larger is almost certainly a typo.
	if count > practicalFanoutLimit {
		return pp, fmt.Errorf("sequential_eoas: count=%d exceeds practical limit (2^32)", count)
	}
	pp.count = count

	// Default 1 wei is the minimum value that keeps a code-less,
	// nonce-0 EOA off EIP-161's pruning path.
	pp.balance = uint256.NewInt(1)
	if v, has := params["balance"]; has {
		b, err := ParseUint256Param(v, "balance")
		if err != nil {
			return pp, fmt.Errorf("sequential_eoas: %w", err)
		}
		if b.IsZero() {
			return pp, fmt.Errorf("sequential_eoas: balance must be > 0 (zero-balance EOAs are pruned by EIP-161)")
		}
		pp.balance = b
	}
	return pp, nil
}

func (sequentialEOAsTemplate) ValidateParameters(params map[string]any) error {
	_, err := parseSequentialEOAsParams(params)
	return err
}

func (sequentialEOAsTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	pp, err := parseSequentialEOAsParams(e.Parameters)
	if err != nil {
		return nil, err
	}

	// Address space is 160 bits. Treat the resolved address as a
	// big-endian integer; reject ranges that walk off the end.
	// (Stays here, not in the parse func: it depends on ctx.)
	startInt := new(big.Int).SetBytes(ctx.ResolvedAddress[:])
	end := new(big.Int).Add(startInt, new(big.Int).SetUint64(pp.count-1))
	if end.BitLen() > 160 {
		return nil, fmt.Errorf("sequential_eoas: range overflows 20 bytes (start=%s, count=%d)",
			ctx.ResolvedAddress.Hex(), pp.count)
	}

	out := make([]PreAllocEntity, pp.count)
	cur := new(big.Int).Set(startInt)
	one := big.NewInt(1)
	for i := uint64(0); i < pp.count; i++ {
		var addr common.Address
		b := cur.Bytes()
		copy(addr[common.AddressLength-len(b):], b)
		out[i] = PreAllocEntity{
			Address: addr,
			Account: &types.StateAccount{
				Nonce:    0,
				Balance:  new(uint256.Int).Set(pp.balance),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash[:],
			},
		}
		cur.Add(cur, one)
	}
	return out, nil
}
