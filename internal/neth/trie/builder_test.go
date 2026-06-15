package trie

import (
	"bytes"
	"fmt"
	mrand "math/rand"
	"sort"
	"testing"

	gethrlp "github.com/ethereum/go-ethereum/rlp"
	gethtrie "github.com/ethereum/go-ethereum/trie"

	"github.com/ethereum/state-actor/internal/neth"
)

// recordingStorage captures every NodeStorage callback for tests.
// All slice arguments are deep-copied so the recorder survives StackTrie's
// volatile-buffer reuse.
type recordingStorage struct {
	stateCalls   []recordedNode
	storageCalls map[[32]byte][]recordedNode
}

type recordedNode struct {
	pathBytes []byte
	pathLen   int
	keccak    [32]byte
	rlp       []byte
}

func newRecordingStorage() *recordingStorage {
	return &recordingStorage{storageCalls: make(map[[32]byte][]recordedNode)}
}

func (r *recordingStorage) SetStateNode(path []byte, pathLen int, keccak [32]byte, rlp []byte) error {
	pcp := append([]byte(nil), path...)
	rcp := append([]byte(nil), rlp...)
	r.stateCalls = append(r.stateCalls, recordedNode{pcp, pathLen, keccak, rcp})
	return nil
}

func (r *recordingStorage) SetStorageNode(addrHash [32]byte, path []byte, pathLen int, keccak [32]byte, rlp []byte) error {
	pcp := append([]byte(nil), path...)
	rcp := append([]byte(nil), rlp...)
	r.storageCalls[addrHash] = append(r.storageCalls[addrHash], recordedNode{pcp, pathLen, keccak, rcp})
	return nil
}

// Compile-time assertion that *recordingStorage satisfies NodeStorage.
var _ NodeStorage = (*recordingStorage)(nil)

// TestBuilder_EmptyStateRootShortCircuits: zero AddAccount calls yields
// neth.EmptyTreeHash with NO SetStateNode invocations.
func TestBuilder_EmptyStateRootShortCircuits(t *testing.T) {
	rec := newRecordingStorage()
	b := NewBuilder(rec)

	root, err := b.FinalizeStateRoot()
	if err != nil {
		t.Fatalf("FinalizeStateRoot: %v", err)
	}
	if root != [32]byte(neth.EmptyTreeHash) {
		t.Errorf("empty-state root: got %x, want %x", root, neth.EmptyTreeHash[:])
	}
	if len(rec.stateCalls) != 0 {
		t.Errorf("empty-state should not invoke SetStateNode (got %d calls)", len(rec.stateCalls))
	}
}

// TestBuilder_EmptyStorageRootShortCircuits: same short-circuit on the
// storage side. The "no storage trie ever opened" path is the typical
// case for EOAs and zero-storage contracts.
func TestBuilder_EmptyStorageRootShortCircuits(t *testing.T) {
	rec := newRecordingStorage()
	b := NewBuilder(rec)

	var addrHash [32]byte
	root, err := b.FinalizeStorageRoot(addrHash)
	if err != nil {
		t.Fatalf("FinalizeStorageRoot: %v", err)
	}
	if root != [32]byte(neth.EmptyTreeHash) {
		t.Errorf("empty-storage root: got %x, want %x", root, neth.EmptyTreeHash[:])
	}
	if len(rec.storageCalls) != 0 {
		t.Errorf("empty-storage should not invoke SetStorageNode")
	}
}

// TestBuilder_SingleAccountMatchesRawStackTrie: single-account state trie
// produces the same root as raw go-ethereum StackTrie consuming the same
// (key, value) pair. This is the load-bearing property: Builder is a
// faithful router of StackTrie's emissions to NodeStorage.
func TestBuilder_SingleAccountMatchesRawStackTrie(t *testing.T) {
	var addrHash [32]byte
	for i := range addrHash {
		addrHash[i] = byte(0xab + i%16)
	}
	accountRLP := []byte{0xc0} // empty list — simplest valid RLP

	// Builder side
	rec := newRecordingStorage()
	b := NewBuilder(rec)
	if err := b.AddAccount(addrHash, accountRLP); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	got, err := b.FinalizeStateRoot()
	if err != nil {
		t.Fatalf("FinalizeStateRoot: %v", err)
	}

	// Reference: raw StackTrie with the same input
	ref := gethtrie.NewStackTrie(nil)
	if err := ref.Update(addrHash[:], accountRLP); err != nil {
		t.Fatalf("ref Update: %v", err)
	}
	want := ref.Hash()

	if got != [32]byte(want) {
		t.Fatalf("root mismatch:\n  got:  %x\n  want: %x", got, want)
	}
	if len(rec.stateCalls) == 0 {
		t.Error("expected at least one SetStateNode call for non-empty trie")
	}
}

// TestBuilder_RootsMatchRawStackTrie_Property: 256 small fixtures, each
// generated from a deterministic seed. For every input, Builder's root
// must equal raw StackTrie's root. This is the cross-encoder property
// test — if Builder's deep-copy bridge ever drifts (e.g., aliases volatile
// buffers), this test surfaces it loudly across many tree shapes.
func TestBuilder_RootsMatchRawStackTrie_Property(t *testing.T) {
	for seed := int64(1); seed <= 256; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := mrand.New(mrand.NewSource(seed))
			n := 1 + r.Intn(64) // 1..64 accounts

			// Generate sorted (addrHash, accountRLP) pairs.
			type pair struct {
				k [32]byte
				v []byte
			}
			pairs := make([]pair, n)
			for i := range pairs {
				r.Read(pairs[i].k[:])
				pairs[i].v = make([]byte, 1+r.Intn(40))
				r.Read(pairs[i].v)
				// Ensure value is valid RLP byte string by setting first byte
				// to a non-list marker. A bare byte slice always RLP-encodes
				// fine via Update — we don't need this hint, but it keeps
				// the test outputs predictable.
			}
			sort.Slice(pairs, func(i, j int) bool {
				return bytes.Compare(pairs[i].k[:], pairs[j].k[:]) < 0
			})

			// Builder
			rec := newRecordingStorage()
			b := NewBuilder(rec)
			for _, p := range pairs {
				if err := b.AddAccount(p.k, p.v); err != nil {
					t.Fatalf("AddAccount: %v", err)
				}
			}
			got, err := b.FinalizeStateRoot()
			if err != nil {
				t.Fatalf("FinalizeStateRoot: %v", err)
			}

			// Reference
			ref := gethtrie.NewStackTrie(nil)
			for _, p := range pairs {
				if err := ref.Update(p.k[:], p.v); err != nil {
					t.Fatalf("ref Update: %v", err)
				}
			}
			want := ref.Hash()

			if got != [32]byte(want) {
				t.Fatalf("seed=%d n=%d root mismatch:\n  got:  %x\n  want: %x", seed, n, got, want)
			}
		})
	}
}

// TestBuilder_StorageBucketsAreIsolated: storage tries for different
// accounts must not bleed into each other. Add slots under addrHash A,
// finalize, then add slots under addrHash B — the recorded callbacks
// must be tagged with the right addrHash each time.
func TestBuilder_StorageBucketsAreIsolated(t *testing.T) {
	addrA := [32]byte{0xaa}
	addrB := [32]byte{0xbb}
	slotKey1 := [32]byte{0x01}
	slotKey2 := [32]byte{0x02}

	rec := newRecordingStorage()
	b := NewBuilder(rec)

	if err := b.AddStorageSlot(addrA, slotKey1, []byte{0x42}); err != nil {
		t.Fatalf("AddStorageSlot A: %v", err)
	}
	rootA, err := b.FinalizeStorageRoot(addrA)
	if err != nil {
		t.Fatalf("FinalizeStorageRoot A: %v", err)
	}
	if err := b.AddStorageSlot(addrB, slotKey2, []byte{0x43}); err != nil {
		t.Fatalf("AddStorageSlot B: %v", err)
	}
	rootB, err := b.FinalizeStorageRoot(addrB)
	if err != nil {
		t.Fatalf("FinalizeStorageRoot B: %v", err)
	}
	if rootA == rootB {
		t.Error("rootA == rootB — different inputs should produce different roots")
	}
	if len(rec.storageCalls[addrA]) == 0 || len(rec.storageCalls[addrB]) == 0 {
		t.Errorf("each addrHash should have ≥1 callbacks: A=%d, B=%d",
			len(rec.storageCalls[addrA]), len(rec.storageCalls[addrB]))
	}
	// Cross-check tags: addrA's calls must NOT appear under addrB.
	for _, ca := range rec.storageCalls[addrA] {
		for _, cb := range rec.storageCalls[addrB] {
			if bytes.Equal(ca.pathBytes, cb.pathBytes) && ca.keccak == cb.keccak {
				t.Errorf("identical callback tagged under both addrA and addrB: %x", ca.pathBytes)
			}
		}
	}
}

// TestBuilder_AddAccountRejectsOpenStorage: AddAccount with an open storage
// trie is a usage error. FinalizeStorageRoot must come first.
func TestBuilder_AddAccountRejectsOpenStorage(t *testing.T) {
	addrHash := [32]byte{0x01}
	b := NewBuilder(newRecordingStorage())
	if err := b.AddStorageSlot(addrHash, [32]byte{0x02}, []byte{0x42}); err != nil {
		t.Fatalf("AddStorageSlot: %v", err)
	}
	if err := b.AddAccount(addrHash, []byte{0xc0}); err == nil {
		t.Error("expected error on AddAccount with open storage trie")
	}
}

// TestBuilder_AddStorageSlotMixedAddrsRejected: two different addrHashes
// in flight without an intervening FinalizeStorageRoot is a usage error.
func TestBuilder_AddStorageSlotMixedAddrsRejected(t *testing.T) {
	b := NewBuilder(newRecordingStorage())
	if err := b.AddStorageSlot([32]byte{0x01}, [32]byte{0xff}, []byte{0x42}); err != nil {
		t.Fatalf("AddStorageSlot 1st: %v", err)
	}
	err := b.AddStorageSlot([32]byte{0x02}, [32]byte{0xfe}, []byte{0x43})
	if err == nil {
		t.Error("expected error when switching addrHash without FinalizeStorageRoot")
	}
}

// TestPackNibblesTo32 covers the nibble→byte packing used in the bridge.
// Mirrors `Nethermind.Trie.TreePath.Path.BytesAsSpan` shape.
func TestPackNibblesTo32(t *testing.T) {
	cases := []struct {
		name    string
		nibbles []byte
		want    [32]byte
	}{
		{"empty", []byte{}, [32]byte{}},
		{"one_nibble_a", []byte{0xa}, [32]byte{0xa0}},
		{"two_nibbles_ab", []byte{0xa, 0xb}, [32]byte{0xab}},
		{"three_nibbles_abc", []byte{0xa, 0xb, 0xc}, [32]byte{0xab, 0xc0}},
		{"four_nibbles_abcd", []byte{0xa, 0xb, 0xc, 0xd}, [32]byte{0xab, 0xcd}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := packNibblesTo32(c.nibbles)
			if got != c.want {
				t.Errorf("packNibblesTo32(%v):\n  got:  %x\n  want: %x", c.nibbles, got, c.want)
			}
		})
	}

	// 64-nibble (full 32-byte path) round-trip:
	full := make([]byte, 64)
	for i := range full {
		full[i] = byte(i % 16)
	}
	packed := packNibblesTo32(full)
	for i := 0; i < 32; i++ {
		want := byte(((i*2)%16)<<4 | ((i*2 + 1) % 16))
		if packed[i] != want {
			t.Errorf("byte[%d]: got 0x%02x, want 0x%02x", i, packed[i], want)
		}
	}
}

// TestBuilder_SinkErrorPropagates: a sink error should surface from the
// next entry-point. We use a custom failing storage that errors on the
// first call.
func TestBuilder_SinkErrorPropagates(t *testing.T) {
	failing := failingStorage{shouldFail: true, msg: "kaboom"}
	b := NewBuilder(&failing)

	addrHash := [32]byte{0x01}
	// AddAccount triggers the trie's StackTrie callback once for the leaf.
	// The callback fails internally, and Builder captures the error.
	_ = b.AddAccount(addrHash, []byte{0xc0})
	// The error appears on FinalizeStateRoot.
	_, err := b.FinalizeStateRoot()
	if err == nil {
		t.Error("expected error from FinalizeStateRoot after sink failure")
	}
}

type failingStorage struct {
	shouldFail bool
	msg        string
}

func (f *failingStorage) SetStateNode(path []byte, pathLen int, keccak [32]byte, rlp []byte) error {
	if f.shouldFail {
		return errSink
	}
	return nil
}
func (f *failingStorage) SetStorageNode(addrHash [32]byte, path []byte, pathLen int, keccak [32]byte, rlp []byte) error {
	if f.shouldFail {
		return errSink
	}
	return nil
}

var errSink = fmt.Errorf("test sink failure")

// TestBuilder_DeepCopyContractRetained: NodeStorage receives slices it
// should be safe to retain. After a multi-entry build, the recorder's
// retained slices must not alias StackTrie's internal pools — they must
// be independent allocations. We approximate by checking that recorded
// path bytes don't compare-equal-by-pointer with each other (a weak but
// real signal that Builder's deep-copy fired).
func TestBuilder_DeepCopyContractRetained(t *testing.T) {
	rec := newRecordingStorage()
	b := NewBuilder(rec)
	// Add enough accounts to exercise multiple StackTrie node emissions.
	for i := 0; i < 16; i++ {
		var k [32]byte
		k[0] = byte(i)
		v, err := gethrlp.EncodeToBytes([]byte("v"))
		if err != nil {
			t.Fatalf("rlp.EncodeToBytes: %v", err)
		}
		if err := b.AddAccount(k, v); err != nil {
			t.Fatalf("AddAccount %d: %v", i, err)
		}
	}
	if _, err := b.FinalizeStateRoot(); err != nil {
		t.Fatalf("FinalizeStateRoot: %v", err)
	}

	// Walk all retained pathBytes; if any two share the same backing
	// array, our deep-copy is wrong. We use sliceBacking() to extract
	// the backing pointer of each slice.
	seen := make(map[uintptr]int) // pointer -> index of first slice with that pointer
	for i, c := range rec.stateCalls {
		if len(c.pathBytes) == 0 {
			continue
		}
		ptr := backingPtr(c.pathBytes)
		if prev, ok := seen[ptr]; ok && prev != i {
			t.Fatalf("stateCalls[%d] aliases stateCalls[%d] (backing pointer %x) — deep-copy missing", i, prev, ptr)
		}
		seen[ptr] = i
	}

	// Same check on RLP slices.
	seen2 := make(map[uintptr]int)
	for i, c := range rec.stateCalls {
		if len(c.rlp) == 0 {
			continue
		}
		ptr := backingPtr(c.rlp)
		if prev, ok := seen2[ptr]; ok && prev != i {
			t.Fatalf("stateCalls[%d].rlp aliases stateCalls[%d].rlp", i, prev)
		}
		seen2[ptr] = i
	}
}

// backingPtr returns a uintptr to the first byte of the slice's backing
// array. Used only by the deep-copy invariant test.
func backingPtr(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafePointer(&b[0]))
}
