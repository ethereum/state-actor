package ethrex

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestSuppressEmptyTrieSentinel_EmptyDropsRowKeepsRoot verifies that an empty
// trie built through the suppressing sink emits ZERO rows (the (path=[],0x80)
// sentinel is dropped) but still returns EMPTY_TRIE_HASH — the exact behavior
// the streaming storage writer relies on for empty PreAlloc storage.
func TestSuppressEmptyTrieSentinel_EmptyDropsRowKeepsRoot(t *testing.T) {
	var rows []strow
	b := NewBuilder(SuppressEmptyTrieSentinel(captureSink(&rows)))
	root, err := b.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty trie through suppressing sink: want 0 rows, got %d (%x)", len(rows), rows)
	}
	want := common.HexToHash(EmptyTrieHashHex)
	if root != want {
		t.Fatalf("root: got %s want %s", root.Hex(), want.Hex())
	}
}

// TestSuppressEmptyTrieSentinel_NonEmptyUnchanged verifies the suppressing sink
// is a no-op for non-empty tries: every row a plain sink would receive is
// passed through, none dropped (no real node row is ([], 0x80)).
func TestSuppressEmptyTrieSentinel_NonEmptyUnchanged(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5E27))
	for iter := 0; iter < 200; iter++ {
		n := rng.Intn(40) + 1
		m := make(map[common.Hash]struct{}, n)
		keys := make([][]byte, 0, n)
		for len(keys) < n {
			var k common.Hash
			rng.Read(k[:])
			nib := BytesToNibbles(k[:])
			if _, ok := m[k]; ok {
				continue
			}
			m[k] = struct{}{}
			keys = append(keys, nib)
		}
		sortNibbleKeys(keys)

		var plain, suppressed []strow
		bp := NewBuilder(captureSink(&plain))
		bs := NewBuilder(SuppressEmptyTrieSentinel(captureSink(&suppressed)))
		for i, k := range keys {
			val := []byte{byte(i + 1)}
			if err := bp.AddLeaf(k, val); err != nil {
				t.Fatalf("plain AddLeaf: %v", err)
			}
			if err := bs.AddLeaf(k, val); err != nil {
				t.Fatalf("suppressed AddLeaf: %v", err)
			}
		}
		rp, err := bp.Root()
		if err != nil {
			t.Fatalf("plain Root: %v", err)
		}
		rs, err := bs.Root()
		if err != nil {
			t.Fatalf("suppressed Root: %v", err)
		}
		if rp != rs {
			t.Fatalf("iter %d: root differs plain=%s suppressed=%s", iter, rp.Hex(), rs.Hex())
		}
		if len(plain) != len(suppressed) {
			t.Fatalf("iter %d (n=%d): row count differs plain=%d suppressed=%d (a real row was dropped)", iter, n, len(plain), len(suppressed))
		}
		for i := range plain {
			if !bytes.Equal(plain[i].path, suppressed[i].path) || !bytes.Equal(plain[i].val, suppressed[i].val) {
				t.Fatalf("iter %d row %d differs", iter, i)
			}
		}
	}
}

func sortNibbleKeys(keys [][]byte) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && bytes.Compare(keys[j-1], keys[j]) > 0; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
}
