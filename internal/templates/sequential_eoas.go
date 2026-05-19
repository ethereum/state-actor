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
// Backs SequentialAddressLayout in execution-specs bloatnet benchmarks:
// the test code iterates address = start + i for i in 0..N; this template
// pre-plants matching state at every such address.
type sequentialEOAsTemplate struct{}

func (sequentialEOAsTemplate) Name() string      { return "sequential_eoas" }
func (sequentialEOAsTemplate) UserVisible() bool { return true }

func (sequentialEOAsTemplate) ValidateParameters(params map[string]any) error {
	if err := RejectUnknownKeys(params, "sequential_eoas", []string{"count", "balance"}); err != nil {
		return err
	}
	if _, ok := params["count"]; !ok {
		return fmt.Errorf("sequential_eoas: missing required parameter `count`")
	}
	if _, err := ParseUint64Param(params["count"], "count"); err != nil {
		return fmt.Errorf("sequential_eoas: %w", err)
	}
	if v, has := params["balance"]; has {
		if _, err := ParseUint256Param(v, "balance"); err != nil {
			return fmt.Errorf("sequential_eoas: %w", err)
		}
	}
	return nil
}

func (sequentialEOAsTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	count, err := ParseUint64Param(e.Parameters["count"], "count")
	if err != nil {
		return nil, fmt.Errorf("sequential_eoas: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	// Practical ceiling: 2^32 entries is already ~128 GB of accounts at
	// the lightest client's bytes-per-account; anything larger is almost
	// certainly a typo.
	if count > 1<<32 {
		return nil, fmt.Errorf("sequential_eoas: count=%d exceeds practical limit (2^32)", count)
	}

	balance := uint256.NewInt(0)
	if v, has := e.Parameters["balance"]; has {
		b, err := ParseUint256Param(v, "balance")
		if err != nil {
			return nil, fmt.Errorf("sequential_eoas: %w", err)
		}
		balance = b
	}

	// Address space is 160 bits. Treat the resolved address as a
	// big-endian integer; reject ranges that walk off the end.
	startInt := new(big.Int).SetBytes(ctx.ResolvedAddress[:])
	end := new(big.Int).Add(startInt, new(big.Int).SetUint64(count-1))
	if end.BitLen() > 160 {
		return nil, fmt.Errorf("sequential_eoas: range overflows 20 bytes (start=%s, count=%d)",
			ctx.ResolvedAddress.Hex(), count)
	}

	out := make([]PreAllocEntity, count)
	cur := new(big.Int).Set(startInt)
	one := big.NewInt(1)
	for i := uint64(0); i < count; i++ {
		var addr common.Address
		b := cur.Bytes()
		copy(addr[common.AddressLength-len(b):], b)
		out[i] = PreAllocEntity{
			Address: addr,
			Account: &types.StateAccount{
				Nonce:    0,
				Balance:  new(uint256.Int).Set(balance),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash[:],
			},
		}
		cur.Add(cur, one)
	}
	return out, nil
}
