package generator

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/state-actor/internal/progress"
)

// sliceIter is a minimal forward-only ethdb.Iterator over fixed (key, value)
// pairs — just enough to exercise tickingIterator's forwarding contract.
type sliceIter struct {
	keys, vals [][]byte
	pos        int
}

func (s *sliceIter) Next() bool    { s.pos++; return s.pos <= len(s.keys) }
func (s *sliceIter) Error() error  { return nil }
func (s *sliceIter) Key() []byte   { return s.keys[s.pos-1] }
func (s *sliceIter) Value() []byte { return s.vals[s.pos-1] }
func (s *sliceIter) Release()      {}

var _ ethdb.Iterator = (*sliceIter)(nil)

// TestTickingIteratorForwardsVerbatim is the determinism guard: the Phase-2
// heartbeat wrapper must yield exactly the same keys/values in the same order
// as the underlying iterator, so the computed root is unchanged. Emission
// throttling itself is covered by internal/progress's own tests.
func TestTickingIteratorForwardsVerbatim(t *testing.T) {
	keys := [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")}
	vals := [][]byte{[]byte("1"), []byte("22"), []byte("333")}

	it := &tickingIterator{Iterator: &sliceIter{keys: keys, vals: vals}, progress: progress.New(), total: int64(len(keys))}

	var gotK, gotV [][]byte
	for it.Next() {
		// Copy: real iterators alias internal buffers, so callers must not
		// retain the slices; copying here mirrors that contract.
		gotK = append(gotK, append([]byte(nil), it.Key()...))
		gotV = append(gotV, append([]byte(nil), it.Value()...))
	}

	if it.Next() { // exhausted iterator must keep returning false
		t.Fatal("Next() returned true after exhaustion")
	}
	if len(gotK) != len(keys) {
		t.Fatalf("yielded %d entries, want %d", len(gotK), len(keys))
	}
	for i := range keys {
		if !bytes.Equal(gotK[i], keys[i]) || !bytes.Equal(gotV[i], vals[i]) {
			t.Fatalf("entry %d = (%q,%q), want (%q,%q)", i, gotK[i], gotV[i], keys[i], vals[i])
		}
	}
	if it.n != int64(len(keys)) {
		t.Fatalf("counter n = %d, want %d", it.n, len(keys))
	}
}

// TestTickingIteratorNilReporter confirms a nil reporter is a no-op on the hot
// path (library/test callers leave Progress nil).
func TestTickingIteratorNilReporter(t *testing.T) {
	keys := [][]byte{[]byte("k")}
	it := &tickingIterator{Iterator: &sliceIter{keys: keys, vals: keys}, total: 1}
	// Force the stride boundary so the (nil) Tick path is exercised.
	it.n = phase2TickStride - 1
	if !it.Next() {
		t.Fatal("Next() should yield the single entry")
	}
}

// TestTickingIteratorCountOnly exercises the total<=0 (count-only) construction
// — used when the entry total is unknown — and confirms forwarding is identical
// to the known-total case and the stride-boundary Tick path doesn't panic.
func TestTickingIteratorCountOnly(t *testing.T) {
	keys := [][]byte{[]byte("x"), []byte("y")}
	it := &tickingIterator{Iterator: &sliceIter{keys: keys, vals: keys}, progress: progress.New(), total: 0}
	it.n = phase2TickStride - 1 // next advance crosses the stride boundary
	var got int
	for it.Next() {
		if !bytes.Equal(it.Key(), keys[got]) {
			t.Fatalf("entry %d key = %q, want %q", got, it.Key(), keys[got])
		}
		got++
	}
	if got != len(keys) {
		t.Fatalf("yielded %d entries, want %d", got, len(keys))
	}
}
