package geth

import (
	"context"
	"iter"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestSpecStorageTrieNodesPersisted pins that the spec-entity Phase 0
// StackTrie emits node writes under TrieNodeStoragePrefix + addrHash.
// Without these, eth_getProof and snapshot regeneration fail at runtime
// even though eth_call (snapshot fast-path) still works.
func TestSpecStorageTrieNodesPersisted(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000abcd")
	addrHash := crypto.Keccak256Hash(addr[:])

	storage := map[common.Hash]common.Hash{
		common.HexToHash("0x01"): common.HexToHash("0xaa"),
		common.HexToHash("0x02"): common.HexToHash("0xbb"),
		common.HexToHash("0x03"): common.HexToHash("0xcc"),
		common.HexToHash("0x04"): common.HexToHash("0xdd"),
		common.HexToHash("0x05"): common.HexToHash("0xee"),
		common.HexToHash("0x06"): common.HexToHash("0xff"),
		common.HexToHash("0x07"): common.HexToHash("0x11"),
		common.HexToHash("0x08"): common.HexToHash("0x22"),
		common.HexToHash("0x09"): common.HexToHash("0x33"),
		common.HexToHash("0x0a"): common.HexToHash("0x44"),
	}

	// Pure-spec test: no AutoFill, just the PreAlloc entity below.
	cfg := generator.Config{
		DBPath:         filepath.Join(t.TempDir(), "geth", "chaindata"),
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
		PreAlloc: []templates.PreAllocEntity{{
			Address: addr,
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(1_000_000),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash[:],
			},
			Storage: storageIterFromMap(storage),
		}},
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if (stats.StateRoot == common.Hash{}) {
		t.Fatal("state root is zero — Populate produced no state")
	}

	w, err := NewWriter(cfg.DBPath)
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	defer w.Close()

	prefix := append([]byte{}, rawdb.TrieNodeStoragePrefix...)
	prefix = append(prefix, addrHash[:]...)

	it := w.DB().NewIterator(prefix, nil)
	defer it.Release()

	count := 0
	for it.Next() {
		count++
	}
	if count == 0 {
		t.Fatalf("no TrieNodeStoragePrefix rows for spec entity addrHash=%s",
			addrHash.Hex())
	}
	t.Logf("spec entity %s has %d storage trie node rows persisted", addr.Hex(), count)
}

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
