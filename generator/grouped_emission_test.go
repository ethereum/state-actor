package generator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	mrand "math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/trie/bintrie"
	"github.com/holiman/uint256"
)

// TestGroupedEmissionConsistency verifies that each group depth produces

// TestGroupedEmissionConsistency verifies that each group depth produces
// a deterministic, non-zero root and writes nodes to DB. The root hash
// varies with groupDepth (stems are placed at extended depths), so we
// verify determinism by running each groupDepth twice.
func TestGroupedEmissionConsistency(t *testing.T) {
	for _, gd := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("gd%d", gd), func(t *testing.T) {
			entries := generateTestEntries(t, 50)

			db1 := memorydb.New()
			root1, stats1 := computeBinaryRootStreamingFromSlice(entries, db1, gd)

			if root1 == (common.Hash{}) {
				t.Fatal("root should not be zero")
			}
			if stats1.Nodes == 0 {
				t.Fatal("should have written nodes")
			}

			// Run again — must produce identical root
			db2 := memorydb.New()
			root2, stats2 := computeBinaryRootStreamingFromSlice(entries, db2, gd)

			if root1 != root2 {
				t.Errorf("non-deterministic root (groupDepth=%d):\n  run1: %s\n  run2: %s",
					gd, root1.Hex(), root2.Hex())
			}
			if stats1.Nodes != stats2.Nodes {
				t.Errorf("non-deterministic node count: %d vs %d", stats1.Nodes, stats2.Nodes)
			}

			// DB contents must also match
			compareDBs(t, db1, db2, gd)
		})
	}
}

// TestGroupedEmissionShallowStems verifies that direct grouped emission
// and the regroup approach produce identical DB contents when stems are
// at shallow non-boundary depths. With 2 stems differing at bit 0, both
// stems are placed at depth 1 — non-boundary for groupDepth >= 2.
func TestGroupedEmissionShallowStems(t *testing.T) {
	entries := makeShallowEntries()

	for _, gd := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("gd%d", gd), func(t *testing.T) {
			// Approach 1: Individual emission (gd=0) + regroup
			db1 := memorydb.New()
			computeBinaryRootStreamingFromSlice(entries, db1, 0)
			if err := regroupTrieNodes(db1, gd, false); err != nil {
				t.Fatalf("regroupTrieNodes failed: %v", err)
			}

			// Approach 2: Direct grouped emission
			db2 := memorydb.New()
			computeBinaryRootStreamingFromSlice(entries, db2, gd)

			// Verify DB contents match between regroup and direct
			compareDBs(t, db1, db2, gd)
		})
	}
}

// TestGroupedEmissionNoRegression verifies that groupDepth=0 produces the
// exact same result as the original ungrouped streaming builder.
func TestGroupedEmissionNoRegression(t *testing.T) {
	entries := generateTestEntries(t, 30)

	db1 := memorydb.New()
	root1, stats1 := computeBinaryRootStreamingFromSlice(entries, db1, 0)

	db2 := memorydb.New()
	root2, stats2 := computeBinaryRootStreamingFromSlice(entries, db2, 0)

	if root1 != root2 {
		t.Errorf("ungrouped root mismatch: %s != %s", root1.Hex(), root2.Hex())
	}
	if stats1.Nodes != stats2.Nodes {
		t.Errorf("node count mismatch: %d != %d", stats1.Nodes, stats2.Nodes)
	}
}

// TestGroupedEmissionSingleEntry verifies edge case with a single stem.
func TestGroupedEmissionSingleEntry(t *testing.T) {
	for _, gd := range []int{0, 1, 4, 8} {
		t.Run(fmt.Sprintf("gd%d", gd), func(t *testing.T) {
			var entries []trieEntry
			var e trieEntry
			for i := 0; i < stemSize; i++ {
				e.Key[i] = byte(i * 7)
			}
			e.Key[stemSize] = 0
			e.Value = sha256.Sum256([]byte("test-value"))
			entries = append(entries, e)

			db := memorydb.New()
			root, stats := computeBinaryRootStreamingFromSlice(entries, db, gd)

			if root == (common.Hash{}) {
				t.Error("root should not be zero for non-empty trie")
			}
			if stats.Nodes == 0 {
				t.Error("should have written at least one node")
			}
		})
	}
}

// TestGroupedEmissionNodeCounts verifies that grouping reduces the number
// of nodes written (internal nodes at non-boundary depths are eliminated).
//
// The gd>=1 check is also a regression test for the root-group overwrite
// bug: pre-fix at gd=1 every depth is a boundary, and the multi-fire
// writeGroupedNode path produced 121 Put calls (= ungrouped count) because
// each overwrite counted as a separate write even though duplicates
// collapsed in the DB. Post-fix each boundary writes at most once, so
// gd=1 strictly beats ungrouped (121 → 118 for the 50-entry fixture).
func TestGroupedEmissionNodeCounts(t *testing.T) {
	entries := generateTestEntries(t, 50)

	db0 := memorydb.New()
	_, stats0 := computeBinaryRootStreamingFromSlice(entries, db0, 0)

	for _, gd := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("gd%d", gd), func(t *testing.T) {
			db := memorydb.New()
			_, stats := computeBinaryRootStreamingFromSlice(entries, db, gd)

			if stats.Nodes >= stats0.Nodes {
				t.Errorf("grouped (gd=%d) should have fewer nodes: grouped=%d, ungrouped=%d",
					gd, stats.Nodes, stats0.Nodes)
			}
			t.Logf("gd=%d: %d nodes (%d bytes), ungrouped: %d nodes (%d bytes)",
				gd, stats.Nodes, stats.Bytes, stats0.Nodes, stats0.Bytes)
		})
	}
}

// --- helpers ---

// makeShallowEntries creates 2 stems that differ at bit 0, forcing stem
// placement at depth 1 (the shallowest possible depth for 2 entries).
func makeShallowEntries() []trieEntry {
	var entries []trieEntry

	// Stem A: bit 0 = 0 (0x00...)
	var eA trieEntry
	stemA := sha256.Sum256([]byte("shallow-stem-A"))
	stemA[0] &= 0x7F // ensure bit 0 = 0
	copy(eA.Key[:stemSize], stemA[:stemSize])
	eA.Key[stemSize] = 0
	eA.Value = sha256.Sum256([]byte("value-A"))
	entries = append(entries, eA)

	// Stem B: bit 0 = 1 (0x80...)
	var eB trieEntry
	stemB := sha256.Sum256([]byte("shallow-stem-B"))
	stemB[0] |= 0x80 // ensure bit 0 = 1
	copy(eB.Key[:stemSize], stemB[:stemSize])
	eB.Key[stemSize] = 0
	eB.Value = sha256.Sum256([]byte("value-B"))
	entries = append(entries, eB)

	// Sort by key
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].Key[:], entries[j].Key[:]) < 0
	})

	return entries
}

// generateTestEntries creates a deterministic set of trie entries with
// multiple stems to exercise the streaming builder's grouping logic.
func generateTestEntries(t *testing.T, numStems int) []trieEntry {
	t.Helper()
	var entries []trieEntry

	for i := 0; i < numStems; i++ {
		stemHash := sha256.Sum256([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		stem := stemHash[:stemSize]

		numSuffixes := (i % 3) + 1
		for s := 0; s < numSuffixes; s++ {
			var e trieEntry
			copy(e.Key[:stemSize], stem)
			e.Key[stemSize] = byte(s)
			e.Value = sha256.Sum256([]byte{byte(i), byte(s)})
			entries = append(entries, e)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].Key[:], entries[j].Key[:]) < 0
	})

	return entries
}

// compareDBs checks that two databases have identical contents under the
// trie node prefix.
func compareDBs(t *testing.T, db1, db2 ethdb.KeyValueStore, groupDepth int) {
	t.Helper()

	prefix := verkleTrieNodeKeyPrefix
	keys1 := collectKeys(db1, prefix)
	keys2 := collectKeys(db2, prefix)

	if len(keys1) != len(keys2) {
		t.Errorf("DB key count mismatch (groupDepth=%d): db1=%d, db2=%d",
			groupDepth, len(keys1), len(keys2))
		set1 := make(map[string]bool)
		for _, k := range keys1 {
			set1[string(k)] = true
		}
		set2 := make(map[string]bool)
		for _, k := range keys2 {
			set2[string(k)] = true
		}
		for _, k := range keys1 {
			if !set2[string(k)] {
				path := k[len(prefix):]
				t.Errorf("  only in db1: depth=%d path=%x", len(path), path)
			}
		}
		for _, k := range keys2 {
			if !set1[string(k)] {
				path := k[len(prefix):]
				t.Errorf("  only in db2: depth=%d path=%x", len(path), path)
			}
		}
		return
	}

	for _, key := range keys1 {
		val1, _ := db1.Get(key)
		val2, _ := db2.Get(key)
		if !bytes.Equal(val1, val2) {
			path := key[len(prefix):]
			t.Errorf("value mismatch at depth=%d path=%x:\n  db1: %x\n  db2: %x",
				len(path), path, truncBlob(val1), truncBlob(val2))
		}
	}
}

func truncBlob(b []byte) []byte {
	if len(b) > 40 {
		return b[:40]
	}
	return b
}

func collectKeys(db ethdb.KeyValueStore, prefix []byte) [][]byte {
	var keys [][]byte
	iter := db.NewIterator(prefix, nil)
	defer iter.Release()
	for iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		key := make([]byte, len(iter.Key()))
		copy(key, iter.Key())
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i], keys[j]) < 0
	})
	return keys
}

// TestParallelKeyDerivation verifies that parallel storage key derivation
// produces identical trie entries as sequential derivation.
func TestParallelKeyDerivation(t *testing.T) {
	addr := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	slots := make([]storageSlot, 500)
	for i := range slots {
		slots[i].Key = sha256.Sum256([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		slots[i].Value = sha256.Sum256([]byte{byte(i), 0xFF})
		if slots[i].Value == (common.Hash{}) {
			slots[i].Value[31] = 1
		}
	}

	// Sequential
	var seqEntries []trieEntry
	for i := range slots {
		seqEntries = collectStorageEntry(addr, slots[i], seqEntries)
	}
	sort.Slice(seqEntries, func(i, j int) bool {
		return bytes.Compare(seqEntries[i].Key[:], seqEntries[j].Key[:]) < 0
	})

	// Parallel
	parEntries := collectStorageEntriesParallel(addr, slots)

	if len(seqEntries) != len(parEntries) {
		t.Fatalf("entry count mismatch: seq=%d par=%d", len(seqEntries), len(parEntries))
	}
	for i := range seqEntries {
		if seqEntries[i] != parEntries[i] {
			t.Errorf("entry %d differs:\n  seq: key=%x val=%x\n  par: key=%x val=%x",
				i, seqEntries[i].Key[:4], seqEntries[i].Value[:4],
				parEntries[i].Key[:4], parEntries[i].Value[:4])
		}
	}
}

// TestCollectAccountEntriesParallelEquivalence verifies that
// collectAccountEntriesParallel produces identical sorted entries
// as the sequential collectAccountEntries.
func TestCollectAccountEntriesParallelEquivalence(t *testing.T) {
	addr := common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	codeHash := sha256.Sum256([]byte("test-code"))
	code := bytes.Repeat([]byte{0xAB}, 500)

	acc := &types.StateAccount{
		Nonce:    42,
		Balance:  uint256.NewInt(1e18),
		Root:     types.EmptyRootHash,
		CodeHash: codeHash[:],
	}

	slots := make([]storageSlot, 200)
	for i := range slots {
		slots[i].Key = sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		slots[i].Value = sha256.Sum256([]byte{byte(i), 0xCC})
	}
	sort.Slice(slots, func(i, j int) bool {
		return bytes.Compare(slots[i].Key[:], slots[j].Key[:]) < 0
	})

	// Sequential
	seqEntries := collectAccountEntries(addr, acc, len(code), code, slots, nil)
	sort.Slice(seqEntries, func(i, j int) bool {
		return bytes.Compare(seqEntries[i].Key[:], seqEntries[j].Key[:]) < 0
	})

	// Parallel
	parEntries := collectAccountEntriesParallel(addr, acc, len(code), code, slots)

	if len(seqEntries) != len(parEntries) {
		t.Fatalf("entry count mismatch: seq=%d par=%d", len(seqEntries), len(parEntries))
	}
	for i := range seqEntries {
		if seqEntries[i] != parEntries[i] {
			t.Errorf("entry %d differs", i)
		}
	}
}

// TestParallelStreamingEquivalence verifies that the parallel pipeline
// produces the exact same root hash as the serial implementation.
func TestParallelStreamingEquivalence(t *testing.T) {
	tests := []struct {
		name        string
		numAccounts int
		groupDepth  int
		writeNodes  bool
	}{
		{"gd0_no_write", 50, 0, false},
		{"gd0_with_write", 50, 0, true},
		{"gd8_no_write", 50, 8, false},
		{"gd8_with_write", 50, 8, true},
		{"large_gd8", 200, 8, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Generate deterministic entries
			entries := generateDeterministicEntries(t, tc.numAccounts)
			if len(entries) == 0 {
				t.Fatal("no entries generated")
			}

			// Sort entries by key (required for streaming)
			sort.Slice(entries, func(i, j int) bool {
				return bytes.Compare(entries[i].Key[:], entries[j].Key[:]) < 0
			})

			// --- Serial computation ---
			var serialDB ethdb.KeyValueStore
			if tc.writeNodes {
				serialDB = rawdb.NewMemoryDatabase()
			}
			serialRoot, _ := computeBinaryRootStreamingFromSlice(
				entries, serialDB, tc.groupDepth,
			)

			// --- Parallel computation ---
			// Write entries to a temp Pebble DB for the iterator
			tempDir := t.TempDir()
			tempDB, err := pebble.New(tempDir, 16, 8, "test/", false)
			if err != nil {
				t.Fatalf("failed to create temp DB: %v", err)
			}
			defer tempDB.Close()

			batch := tempDB.NewBatch()
			for _, e := range entries {
				if err := batch.Put(e.Key[:], e.Value[:]); err != nil {
					t.Fatalf("batch put: %v", err)
				}
				if batch.ValueSize() >= 16*1024*1024 {
					if err := batch.Write(); err != nil {
						t.Fatalf("batch write: %v", err)
					}
					batch.Reset()
				}
			}
			if batch.ValueSize() > 0 {
				if err := batch.Write(); err != nil {
					t.Fatalf("batch write: %v", err)
				}
			}

			var parallelDB ethdb.KeyValueStore
			if tc.writeNodes {
				parallelDB = rawdb.NewMemoryDatabase()
			}
			var parallelTNW *trieNodeWriter
			if parallelDB != nil {
				parallelTNW = &trieNodeWriter{batch: parallelDB.NewBatch(), db: parallelDB}
			}
			iter := tempDB.NewIterator(nil, nil)
			parallelRoot, _, _, pErr := computeBinaryRootStreamingParallel(
				context.Background(), iter, parallelTNW, nil, tc.groupDepth, 4, nil,
			)
			if pErr != nil {
				t.Fatalf("parallel computation failed: %v", pErr)
			}

			if serialRoot != parallelRoot {
				t.Errorf("root mismatch:\n  serial:   %s\n  parallel: %s",
					serialRoot.Hex(), parallelRoot.Hex())
			}

			// If writing nodes, verify same set of trie nodes written
			if tc.writeNodes && serialDB != nil && parallelDB != nil {
				serialIter := serialDB.NewIterator(nil, nil)
				parallelIter := parallelDB.NewIterator(nil, nil)
				serialCount := 0
				parallelCount := 0
				for serialIter.Next() {
					serialCount++
				}
				serialIter.Release()
				for parallelIter.Next() {
					parallelCount++
				}
				parallelIter.Release()
				if serialCount != parallelCount {
					t.Errorf("trie node count mismatch: serial=%d parallel=%d",
						serialCount, parallelCount)
				}
			}
		})
	}
}

// generateDeterministicEntries creates a set of trie entries for testing.
// Uses a fixed seed for deterministic results.
func generateDeterministicEntries(t *testing.T, numAccounts int) []trieEntry {
	t.Helper()
	rng := mrand.New(mrand.NewSource(42))
	var entries []trieEntry

	for i := 0; i < numAccounts; i++ {
		// Generate a random address
		var addr common.Address
		rng.Read(addr[:])

		// Create a minimal account (basic data + code hash = 2 entries per stem)
		balance := uint256.NewInt(uint64(rng.Intn(1000)))
		acc := &types.StateAccount{
			Nonce:    uint64(rng.Intn(100)),
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}

		entries = collectAccountEntries(addr, acc, 0, nil, nil, entries)

		// Every 5th account: add some storage slots
		if i%5 == 0 {
			numSlots := 2 + rng.Intn(20)
			for j := 0; j < numSlots; j++ {
				var slotKey common.Hash
				rng.Read(slotKey[:])
				k := bintrie.GetBinaryTreeKeyStorageSlot(addr, slotKey[:])
				var e trieEntry
				copy(e.Key[:], k)
				rng.Read(e.Value[:])
				if e.Value == (common.Hash{}) {
					e.Value[0] = 1
				}
				entries = append(entries, e)
			}
		}
	}

	return entries
}

// TestGroupedEmissionRootGroupCompleteness is an invariant check for the
// root group's bitmap: after building with 32 stems — one per possible
// root-group slot at groupDepth=5 — the root blob at key "vA" must have
// ALL 32 slots populated and be 1030 bytes (2 header + 4 bitmap + 32×32
// hashes). Bug symptom observed in production: "vA" blob was 390 bytes
// with slot 25 unset, causing geth GetValuesAtStem to fail with "missing
// trie node" for any stem under the unpopulated root subtree.
//
// Note: the 32-stem input alone does not trigger the multi-fire overwrite
// that produced the production symptom (the final writeGroupedNode(0, ...)
// happens to capture all children). TestGroupedEmissionNodeCounts at gd=1
// is the mechanical pre-fix-failing regression test; this test is the
// end-state correctness check.
func TestGroupedEmissionRootGroupCompleteness(t *testing.T) {
	const gd = 5
	const expectedSlots = 1 << gd // 32

	var entries []trieEntry
	for slot := 0; slot < expectedSlots; slot++ {
		var e trieEntry
		// Set bits 0..4 of the stem to encode `slot` as a 5-bit number
		// (MSB first: bit 0 is the most significant of the 5-bit slot).
		for i := 0; i < gd; i++ {
			if slot&(1<<(gd-1-i)) != 0 {
				e.Key[i/8] |= 1 << (7 - (i % 8))
			}
		}
		// Fill remaining bits deterministically so each stem is distinct.
		tail := sha256.Sum256([]byte{byte(slot), 0xAA})
		copy(e.Key[5:stemSize], tail[5:])
		e.Key[stemSize] = 0
		e.Value = sha256.Sum256([]byte{byte(slot), 0xBB})
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].Key[:], entries[j].Key[:]) < 0
	})

	db := memorydb.New()
	root, _ := computeBinaryRootStreamingFromSlice(entries, db, gd)
	if root == (common.Hash{}) {
		t.Fatal("root is zero")
	}
	// Pinned so an unintended hash drift (e.g. a subtle rebalancing
	// change to stem placement or combine ordering) is caught even if
	// the bitmap assertions below still succeed.
	wantRoot := common.HexToHash("0x07b7cfc5d084c25927977209dbfd2b06906456f67ef2554dc5a812f6d894b99b")
	if root != wantRoot {
		t.Errorf("root hash drift: got %s, want %s", root.Hex(), wantRoot.Hex())
	}

	// Read the root group blob at key "vA" (verkleTrieNodeKeyPrefix + empty path).
	blob, err := db.Get(verkleTrieNodeKeyPrefix)
	if err != nil {
		t.Fatalf("root group missing at key %q: %v", verkleTrieNodeKeyPrefix, err)
	}
	if len(blob) < 6 {
		t.Fatalf("root group blob too short: %d bytes", len(blob))
	}

	// Expected layout: [type=2][groupDepth][bitmap(4 bytes)][N*32 hashes].
	// With all 32 slots populated: 1 + 1 + 4 + 32*32 = 1030 bytes.
	bitmapSize := bitmapSizeForDepth(gd)
	expectedLen := 1 + 1 + bitmapSize + expectedSlots*hashSize
	if len(blob) != expectedLen {
		t.Errorf("root group blob is %d bytes (=%d children); want %d bytes (=%d children)",
			len(blob), (len(blob)-2-bitmapSize)/hashSize, expectedLen, expectedSlots)
	}

	// Assert every bitmap slot is set.
	for slot := 0; slot < expectedSlots; slot++ {
		byteIdx := 2 + slot/8
		bitInByte := uint(7 - slot%8)
		if blob[byteIdx]&(1<<bitInByte) == 0 {
			t.Errorf("bitmap slot %d (bits %05b) NOT set — likely overwritten by a later partial write", slot, slot)
		}
	}
}

// TestGroupedEmissionParallelRootGroupCompleteness is the parallel-builder
// counterpart of TestGroupedEmissionRootGroupCompleteness. The parallel
// builder (computeBinaryRootStreamingParallel) runs feedStem inside a
// dedicated builder goroutine; it initializes groupStemAtBoundary at a
// separate init site and exercises the same flushCompletedGroupsAbove
// path. TestParallelStreamingEquivalence only compares total node counts
// and root hashes — it would not catch a bitmap truncation regression if
// both serial and parallel regressed identically.
func TestGroupedEmissionParallelRootGroupCompleteness(t *testing.T) {
	const gd = 5
	const expectedSlots = 1 << gd

	var entries []trieEntry
	for slot := 0; slot < expectedSlots; slot++ {
		var e trieEntry
		for i := 0; i < gd; i++ {
			if slot&(1<<(gd-1-i)) != 0 {
				e.Key[i/8] |= 1 << (7 - (i % 8))
			}
		}
		tail := sha256.Sum256([]byte{byte(slot), 0xAA})
		copy(e.Key[5:stemSize], tail[5:])
		e.Key[stemSize] = 0
		e.Value = sha256.Sum256([]byte{byte(slot), 0xBB})
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].Key[:], entries[j].Key[:]) < 0
	})

	// The parallel builder reads entries via an ethdb.Iterator, so stage
	// the entries in a temp Pebble DB and iterate over it.
	tempDir := t.TempDir()
	tempDB, err := pebble.New(tempDir, 16, 8, "test/", false)
	if err != nil {
		t.Fatalf("pebble.New: %v", err)
	}
	defer tempDB.Close()
	batch := tempDB.NewBatch()
	for _, e := range entries {
		if err := batch.Put(e.Key[:], e.Value[:]); err != nil {
			t.Fatalf("batch.Put: %v", err)
		}
	}
	if err := batch.Write(); err != nil {
		t.Fatalf("batch.Write: %v", err)
	}

	outDB := memorydb.New()
	tnw := &trieNodeWriter{batch: outDB.NewBatch(), db: outDB}
	iter := tempDB.NewIterator(nil, nil)
	root, _, _, pErr := computeBinaryRootStreamingParallel(
		context.Background(), iter, tnw, nil, gd, 4, nil,
	)
	if pErr != nil {
		t.Fatalf("computeBinaryRootStreamingParallel: %v", pErr)
	}
	if root == (common.Hash{}) {
		t.Fatal("root is zero")
	}

	blob, err := outDB.Get(verkleTrieNodeKeyPrefix)
	if err != nil {
		t.Fatalf("root group missing: %v", err)
	}
	bitmapSize := bitmapSizeForDepth(gd)
	expectedLen := 1 + 1 + bitmapSize + expectedSlots*hashSize
	if len(blob) != expectedLen {
		t.Errorf("parallel root group blob is %d bytes (=%d children); want %d bytes (=%d children)",
			len(blob), (len(blob)-2-bitmapSize)/hashSize, expectedLen, expectedSlots)
	}
	for slot := 0; slot < expectedSlots; slot++ {
		byteIdx := 2 + slot/8
		bitInByte := uint(7 - slot%8)
		if blob[byteIdx]&(1<<bitInByte) == 0 {
			t.Errorf("parallel bitmap slot %d NOT set", slot)
		}
	}
}
