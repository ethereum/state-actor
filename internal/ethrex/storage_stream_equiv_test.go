package ethrex

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/streamingtrie"
)

// storage_stream_equiv_test.go is the byte-identity gate for routing the ethrex
// storage feed through internal/streamingtrie. It proves two things, both
// without cgo/rocksdb:
//
//  1. The value RLP streamingtrie produces (trim leading zeros + rlp.EncodeToBytes)
//     is byte-identical to ethrex's EncodeStorageValue, for every value.
//  2. Building a storage trie via streamingtrie.StorageRoot + StreamHashBuilder
//     emits an identical row set and root to the old materialized algorithm
//     (range → sort → Builder.AddLeaf(EncodeStorageValue)).
//
// If either fails, the streaming writer rewrite is unsafe and must stop.

// streamtrieValueRLP reproduces the exact value encoding in
// streamingtrie.IterateRoot (streamingtrie.go:146-150).
func streamtrieValueRLP(t *testing.T, v common.Hash) []byte {
	t.Helper()
	b := v[:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	out, err := rlp.EncodeToBytes(b)
	if err != nil {
		t.Fatalf("rlp.EncodeToBytes: %v", err)
	}
	return out
}

func TestStorageValueEncodingMatchesStreamingtrie(t *testing.T) {
	check := func(u *uint256.Int) {
		h := common.Hash(u.Bytes32())
		got := EncodeStorageValue(u)
		want := streamtrieValueRLP(t, h)
		if !bytes.Equal(got, want) {
			t.Fatalf("value %s: EncodeStorageValue=%x streamingtrie=%x", u.Hex(), got, want)
		}
	}

	// Boundary values that exercise every RLP branch (single byte <0x80,
	// single byte >=0x80, multi-byte, leading/embedded zeros, max).
	for _, hx := range []string{
		"0x1", "0x7f", "0x80", "0xff", "0x100", "0x1ff", "0x100ff",
		"0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	} {
		check(uint256.MustFromHex(hx))
	}

	rng := rand.New(rand.NewSource(0x57052A6E))
	for i := 0; i < 10000; i++ {
		var b [32]byte
		// Random length of significant bytes so leading-zero stripping varies.
		sig := rng.Intn(32) + 1
		rng.Read(b[32-sig:])
		u := new(uint256.Int).SetBytes(b[:])
		if u.IsZero() {
			continue // zero is never encoded (skipped upstream)
		}
		check(u)
	}
}

type strow struct {
	path []byte
	val  []byte
}

func captureSink(rows *[]strow) NodeSink {
	return func(path, val []byte) error {
		*rows = append(*rows, strow{
			path: append([]byte(nil), path...),
			val:  append([]byte(nil), val...),
		})
		return nil
	}
}

func rowKey(r strow) string { return fmt.Sprintf("%x|%x", r.path, r.val) }

func TestStorageStreamMatchesMaterialized(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5709A6E0))

	for iter := 0; iter < 300; iter++ {
		n := rng.Intn(256)
		m := make(map[common.Hash]common.Hash, n)
		for len(m) < n {
			var k, v common.Hash
			rng.Read(k[:])
			// ~1 in 8 slots is zero, to exercise the skip path on both sides.
			if rng.Intn(8) != 0 {
				sig := rng.Intn(32) + 1
				rng.Read(v[32-sig:])
			}
			m[k] = v
		}
		seq := func(yield func(common.Hash, common.Hash) bool) {
			for k, v := range m {
				if !yield(k, v) {
					return
				}
			}
		}

		// Streaming path.
		var streamRows []strow
		hb := NewStreamHashBuilder(captureSink(&streamRows))
		streamRoot, err := streamingtrie.StorageRoot(t.TempDir(), seq, hb, nil)
		if err != nil {
			t.Fatalf("iter %d: StorageRoot: %v", iter, err)
		}

		// Materialized reference: the old ethrex Phase-0 algorithm.
		type kv struct {
			keyHash common.Hash
			enc     []byte
		}
		var kvs []kv
		for k, v := range m {
			if v == (common.Hash{}) {
				continue
			}
			u := new(uint256.Int).SetBytes(v[:])
			kvs = append(kvs, kv{crypto.Keccak256Hash(k[:]), EncodeStorageValue(u)})
		}
		sort.Slice(kvs, func(i, j int) bool {
			return bytes.Compare(kvs[i].keyHash[:], kvs[j].keyHash[:]) < 0
		})
		var refRows []strow
		rb := NewBuilder(captureSink(&refRows))
		for _, e := range kvs {
			if err := rb.AddLeaf(BytesToNibbles(e.keyHash[:]), e.enc); err != nil {
				t.Fatalf("iter %d: ref AddLeaf: %v", iter, err)
			}
		}
		refRoot, err := rb.Root()
		if err != nil {
			t.Fatalf("iter %d: ref Root: %v", iter, err)
		}

		if streamRoot != refRoot {
			t.Fatalf("iter %d (n=%d): root mismatch stream=%s ref=%s", iter, n, streamRoot.Hex(), refRoot.Hex())
		}

		streamSet := map[string]int{}
		for _, r := range streamRows {
			streamSet[rowKey(r)]++
		}
		refSet := map[string]int{}
		for _, r := range refRows {
			refSet[rowKey(r)]++
		}
		if len(streamSet) != len(refSet) {
			t.Fatalf("iter %d (n=%d): row-set size mismatch stream=%d ref=%d", iter, n, len(streamSet), len(refSet))
		}
		for k, c := range refSet {
			if streamSet[k] != c {
				t.Fatalf("iter %d (n=%d): row mismatch for %s (stream=%d ref=%d)", iter, n, k, streamSet[k], c)
			}
		}
	}
}
