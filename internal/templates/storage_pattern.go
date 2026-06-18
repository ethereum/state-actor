package templates

import (
	"encoding/binary"
	"fmt"
	"iter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func init() {
	Register(&storagePatternTemplate{})
}

// storagePatternTemplate handles `kind: contract, template: storage_pattern`.
// Plants a self-pointer + dense counter storage layout at the entity's
// resolved address:
//
//	slot 0          = final + 1   (next-free index pointer)
//	slot k (1..N)   = k           (identity, for k in 1..final)
//
// No code is set so the address remains 7702-delegatable at run time.
// Nonce is forced to >= 1 (defaulting to 1 if absent) so Spurious
// Dragon+ empty-account pruning doesn't wipe a code-less, balance-0
// entry.
//
// Backs the existing_slots=True path of test_sload_bloated /
// test_sstore_bloated in execution-specs/bloatnet/test_single_opcode.py:
// those tests assume the target's storage matches this exact layout.
type storagePatternTemplate struct{}

// TemplateNameStoragePattern is the registry key for this template.
const TemplateNameStoragePattern = "storage_pattern"

func (storagePatternTemplate) Name() string      { return TemplateNameStoragePattern }
func (storagePatternTemplate) UserVisible() bool { return true }

func (storagePatternTemplate) HonoredEntityFields() EntityFieldSet {
	h := EntityFieldSupport{Honored: true}
	return EntityFieldSet{
		Balance:              h,
		Nonce:                h, // floored to 1 in Expand (EIP-161)
		Code:                 EntityFieldSupport{}, // deliberately code-less so the address stays 7702-delegatable
		ApproximateSizeBytes: EntityFieldSupport{Alternative: "parameters.final"},
	}
}

// storagePatternParams is the typed result of parseStoragePatternParams.
type storagePatternParams struct {
	final uint64 // in [0, 2^32]; final=0 stays legal (emits only slot 0 = 1)
}

// parseStoragePatternParams is the single validation+parse boundary for
// this template. ValidateParameters and Expand both call it, so a
// parameter set that validates is — by construction — the same one that
// expands; no check can drift between the two entry points.
func parseStoragePatternParams(params map[string]any) (storagePatternParams, error) {
	var pp storagePatternParams
	if err := RejectUnknownKeys(params, "storage_pattern", []string{"final"}); err != nil {
		return pp, err
	}
	if _, ok := params["final"]; !ok {
		return pp, fmt.Errorf("storage_pattern: missing required parameter `final`")
	}
	final, err := ParseUint64Param(params["final"], "final")
	if err != nil {
		return pp, fmt.Errorf("storage_pattern: %w", err)
	}
	// Practical ceiling shared with the other fan-out templates — and
	// load-bearing for correctness here: storagePatternIter's
	// `k <= final` loop never terminates at final == MaxUint64, and
	// slot 0 (final+1) would silently wrap to 0.
	if final > practicalFanoutLimit {
		return pp, fmt.Errorf("storage_pattern: final=%d exceeds practical limit (2^32)", final)
	}
	pp.final = final
	return pp, nil
}

func (storagePatternTemplate) ValidateParameters(params map[string]any) error {
	_, err := parseStoragePatternParams(params)
	return err
}

func (storagePatternTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	pp, err := parseStoragePatternParams(e.Parameters)
	if err != nil {
		return nil, err
	}
	final := pp.final

	// Force nonce >= 1. Empty-account pruning (EIP-161) would wipe a
	// code-less, balance-0, nonce-0 account, taking the storage pattern
	// with it.
	nonce := e.Nonce
	if nonce == 0 {
		nonce = 1
	}
	balance := uint256.NewInt(0)
	if e.Balance != nil {
		balance = new(uint256.Int).Set(e.Balance.V)
	}

	pe := PreAllocEntity{
		Address: ctx.ResolvedAddress,
		Account: &types.StateAccount{
			Nonce:    nonce,
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash[:],
		},
		Storage: storagePatternIter(final),
	}
	return []PreAllocEntity{pe}, nil
}

// storagePatternIter yields slot 0 → final+1, then slot k → k for
// k in 1..final. Pure closure, re-iterable.
func storagePatternIter(final uint64) iter.Seq2[common.Hash, common.Hash] {
	return func(yield func(common.Hash, common.Hash) bool) {
		if !yield(common.Hash{}, uint64ToHash(final+1)) {
			return
		}
		for k := uint64(1); k <= final; k++ {
			if !yield(uint64ToHash(k), uint64ToHash(k)) {
				return
			}
		}
	}
}

func uint64ToHash(v uint64) common.Hash {
	var h common.Hash
	binary.BigEndian.PutUint64(h[common.HashLength-8:], v)
	return h
}
