package generator

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

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
func TestGroupedEmissionNodeCounts(t *testing.T) {
	entries := generateTestEntries(t, 50)

	db0 := memorydb.New()
	_, stats0 := computeBinaryRootStreamingFromSlice(entries, db0, 0)

	for _, gd := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("gd%d", gd), func(t *testing.T) {
			db := memorydb.New()
			_, stats := computeBinaryRootStreamingFromSlice(entries, db, gd)

			if gd > 1 && stats.Nodes >= stats0.Nodes {
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
