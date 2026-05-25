//go:build cgo_reth

package reth

import (
	"context"
	"iter"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestStreamSpecStorageParallelDeterminism: drainedCh delivers entities
// in worker-arrival order — same PreAlloc must yield the same state
// root across runs.
func TestStreamSpecStorageParallelDeterminism(t *testing.T) {
	// 32 entities × 30 slots: more entities than maxStreamSpecStorageWorkers
	// so workers process multiple items and drain order genuinely
	// interleaves across runs.
	const numEntities = 32
	const slotsPerEntity = 30

	buildCfg := func(dbPath string) generator.Config {
		preAlloc := make([]templates.PreAllocEntity, numEntities)
		for i := 0; i < numEntities; i++ {
			var addr common.Address
			addr[0] = byte(i + 1)
			addr[19] = 0x01
			storage := make(map[common.Hash]common.Hash, slotsPerEntity)
			for s := 0; s < slotsPerEntity; s++ {
				var key common.Hash
				key[30] = byte(i)
				key[31] = byte(s)
				var val common.Hash
				val[31] = byte(s + 1)
				storage[key] = val
			}
			preAlloc[i] = templates.PreAllocEntity{
				Address: addr,
				Account: &types.StateAccount{
					Nonce:    1,
					Balance:  uint256.NewInt(uint64(1000 + i)),
					Root:     types.EmptyRootHash,
					CodeHash: types.EmptyCodeHash[:],
				},
				Storage: storageIterFromMap(storage),
			}
		}
		return generator.Config{DBPath: dbPath, PreAlloc: preAlloc}
	}

	const runs = 2
	var roots [runs]common.Hash
	emptyRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	for i := 0; i < runs; i++ {
		stats, err := RunCgo(context.Background(), buildCfg(t.TempDir()), Options{})
		if err != nil {
			t.Fatalf("run %d: RunCgo: %v", i, err)
		}
		if stats.StateRoot == (common.Hash{}) || stats.StateRoot == emptyRoot {
			t.Fatalf("run %d: state root %s did not absorb storage", i, stats.StateRoot.Hex())
		}
		roots[i] = stats.StateRoot
	}
	if roots[0] != roots[1] {
		t.Errorf("non-deterministic state root: %s vs %s", roots[0].Hex(), roots[1].Hex())
	}
}

// TestStreamSpecStorageNoStorageEntities: a pure-EOA PreAlloc must skip
// the storage writer loop entirely — stats.StorageBytes stays zero, no
// MDBX txn for storage opens. (The empty-PreAlloc case is now redundant
// — generator.Config.Validate rejects empty configs at the top of
// RunCgo, so the writer loop is unreachable for nil PreAlloc anyway.)
func TestStreamSpecStorageNoStorageEntities(t *testing.T) {
	addr := common.HexToAddress("0xaabbccddeeff00112233445566778899aabbccdd")
	pureEOA := []templates.PreAllocEntity{{
		Address: addr,
		Account: &types.StateAccount{
			Nonce:    0,
			Balance:  uint256.NewInt(1000),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash[:],
		},
		Storage: nil,
	}}

	stats, err := RunCgo(context.Background(), generator.Config{
		DBPath:   t.TempDir(),
		PreAlloc: pureEOA,
	}, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats.StorageBytes != 0 {
		t.Errorf("StorageBytes = %d, want 0", stats.StorageBytes)
	}
}

// storageIterFromMap yields the map's (k, v) pairs in hash-map order;
// the consumer keccak-sorts internally.
func storageIterFromMap(m map[common.Hash]common.Hash) iter.Seq2[common.Hash, common.Hash] {
	if len(m) == 0 {
		return nil
	}
	return func(yield func(common.Hash, common.Hash) bool) {
		for k, v := range m {
			if !yield(k, v) {
				return
			}
		}
	}
}
