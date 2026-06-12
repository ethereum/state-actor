package templates

import (
	"encoding/binary"
	"fmt"
	"iter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
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

func (storagePatternTemplate) ValidateParameters(params map[string]any) error {
	if err := RejectUnknownKeys(params, "storage_pattern", []string{"final"}); err != nil {
		return err
	}
	if _, ok := params["final"]; !ok {
		return fmt.Errorf("storage_pattern: missing required parameter `final`")
	}
	if _, err := ParseUint64Param(params["final"], "final"); err != nil {
		return fmt.Errorf("storage_pattern: %w", err)
	}
	return nil
}

func (storagePatternTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	final, err := ParseUint64Param(e.Parameters["final"], "final")
	if err != nil {
		return nil, fmt.Errorf("storage_pattern: %w", err)
	}

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
