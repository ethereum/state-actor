//go:build btindex_large

// Gated behind `btindex_large` — the Tier-0 bounded-heap regression test
// for the B1 offsets-spill change. It asserts the writer's in-flight RAM
// stays sub-linear in KeyCount: with offsets spilled to disk the only
// per-key growth is the sparse `nodes` cache (~84 B every M=256 keys ≈
// 0.33 B/key) plus a fixed bufio buffer. Before the spill, offsets were an
// in-memory []uint64 (8 B/key), so this test would have caught the OOM.
//
// Run with:
//
//	go test -tags btindex_large -run TestBtindex_BoundedHeap -timeout 5m ./internal/erigon/btindex/...
package btindex

import (
	"path/filepath"
	"runtime"
	"testing"
)

// writerFootprint returns the live-heap delta attributable to one Writer
// after adding n keys (before Build). One key buffer is reused so the test
// harness itself allocates O(1); the writer copies only the kept 1/256.
func writerFootprint(t *testing.T, n int) float64 {
	t.Helper()
	tmp := t.TempDir()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	w, err := New(Args{KeyCount: n, M: 256, TmpDir: tmp, IndexFile: filepath.Join(tmp, "bounded.bt")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := make([]byte, 32)
	var off uint64
	for i := 0; i < n; i++ {
		key[0], key[1], key[2] = byte(i), byte(i>>8), byte(i>>16)
		off += 7
		if err := w.AddKey(key, off); err != nil {
			t.Fatalf("AddKey[%d]: %v", i, err)
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(w) // writer (and its in-flight state) must survive the measurement
	_ = w.Close()

	var footprint float64
	if after.HeapAlloc > before.HeapAlloc {
		footprint = float64(after.HeapAlloc - before.HeapAlloc)
	}
	return footprint / float64(n)
}

func TestBtindex_BoundedHeap(t *testing.T) {
	const n = 5_000_000
	bytesPerKey := writerFootprint(t, n)
	t.Logf("btindex in-flight footprint at %d keys: %.3f B/key", n, bytesPerKey)

	// Spill ⇒ ~0.33 B/key (nodes) + amortized bufio; the old []uint64
	// offsets path was ~8.3 B/key. 2.0 sits well between the two.
	const limit = 2.0
	if bytesPerKey > limit {
		t.Fatalf("in-flight heap %.3f B/key exceeds %.1f — offsets appear held in RAM (B1 regressed)", bytesPerKey, limit)
	}
}
