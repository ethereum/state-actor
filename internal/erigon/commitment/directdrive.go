//go:build cgo_erigon_commitment

package commitment

// The Direct-Drive Fold (DDF): the default commitment path. Feeds the
// vendored engine's followAndUpdate straight from the 16 hashed-keyed
// sub-store cursors (each yields ascending nibblized-keccak keys — the
// engine's sort order, with the Update riding the input row), so there is
// no Touch dedup map, no etl spill, and no per-key ctx Gets. Branch rows
// land in per-worker WRITE-ONCE sinks (compaction off), counted as
// emitted (from-empty, each prefix folds exactly once), and are k-way
// merged in ascending order by Result.BranchIterate.

import (
	"container/heap"
	"context"
	"fmt"
	"os"
	"sync"

	gethcommon "github.com/ethereum/go-ethereum/common"

	erigonkv "github.com/erigontech/erigon/db/kv"

	"github.com/ethereum/state-actor/internal/erigon/hph"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// directDrive gates the DDF path (env STATE_ACTOR_COMMITMENT_DIRECT;
// default ON — set 0 to fall back to the Updates/etl engine path).
var directDrive = func() bool {
	v := os.Getenv("STATE_ACTOR_COMMITMENT_DIRECT")
	return v != "0" && v != "false"
}()

func directEnabled() bool { return directDrive }

// setDirectEnabled swaps the DDF gate for a test; the returned func restores.
func setDirectEnabled(b bool) (restore func()) {
	prev := directDrive
	directDrive = b
	return func() { directDrive = prev }
}

// cursorStream adapts one hashed-keyed sub-store cursor to hph.KeyStream.
// Aliasing contract: Key()/Value() alias pebble buffers valid until the
// cursor advances — we advance at the START of the next Next() call, and the
// engine copies what it keeps (updateCell copies plainKey/hashedKey into the
// cell; Update.Decode copies into the struct) before returning, so the
// previous row's slices are never read after invalidation.
type cursorStream struct {
	cur     *streamsort.Cursor
	started bool
	u       hph.Update
}

func (s *cursorStream) Next() (hashedKey, plainKey []byte, u *hph.Update, ok bool, err error) {
	if s.started {
		s.cur.Next()
	} else {
		s.started = true
	}
	if !s.cur.Valid() {
		return nil, nil, nil, false, s.cur.Err()
	}
	hashedKey = s.cur.Key()
	plainKey, updBytes, derr := DecodeInputRow(s.cur.Value())
	if derr != nil {
		return nil, nil, nil, false, derr
	}
	s.u = hph.Update{} // full reset — Decode only writes flagged fields
	if _, derr := s.u.Decode(updBytes, 0); derr != nil {
		return nil, nil, nil, false, fmt.Errorf("decode update for %x: %w", plainKey, derr)
	}
	return hashedKey, plainKey, &s.u, true, nil
}

// directSinks registers each context's write-once branch sink as it closes,
// capturing the first sink-finalize error for the caller to surface.
type directSinks struct {
	mu       sync.Mutex
	tmpDir   string
	sinks    []*streamsort.Store
	rows     uint64
	firstErr error
}

func (r *directSinks) register(sink *streamsort.Store, rows uint64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sink != nil {
		r.sinks = append(r.sinks, sink)
	}
	r.rows += rows
	if err != nil && r.firstErr == nil {
		r.firstErr = err
	}
}

func (r *directSinks) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstErr
}

// hphDirectCtx is the DDF PatriciaContext: pure write-once branch sink.
//
//   - Branch always returns nil: from-empty single-shot, the unfold path
//     never re-descends a written prefix and the fold's prev-merge read is
//     nil-correct for every first-time fold (each prefix folds exactly once).
//     Golden B pins this byte-for-byte against the Updates/etl engine path.
//   - Account/Storage FAIL LOUDLY: DDF passes the Update inline, so any
//     re-fetch means the from-empty assumption broke — surface it, never
//     serve stale data.
type hphDirectCtx struct {
	reg  *directSinks
	sink *streamsort.Store // lazy — unfoldRoot's provisional ctxs never write
	rows uint64
}

func (c *hphDirectCtx) Branch(prefix []byte) ([]byte, erigonkv.Step, error) {
	return nil, 0, nil
}

func (c *hphDirectCtx) PutBranch(prefix []byte, data []byte, prevData []byte) error {
	if c.sink == nil {
		// Write-once sink drained by one merge scan: 64 MiB arenas suffice.
		s, err := streamsort.NewWithOptions(c.reg.tmpDir, streamsort.Options{MemTableBytes: 64 << 20})
		if err != nil {
			return fmt.Errorf("directdrive: branch sink: %w", err)
		}
		c.sink = s
	}
	c.rows++
	return c.sink.Put(prefix, data)
}

func (c *hphDirectCtx) Account(plainKey []byte) (*hph.Update, error) {
	return nil, fmt.Errorf("directdrive: unexpected ctx.Account(%x) — updates ride the input rows", plainKey)
}

func (c *hphDirectCtx) Storage(plainKey []byte) (*hph.Update, error) {
	return nil, fmt.Errorf("directdrive: unexpected ctx.Storage(%x) — updates ride the input rows", plainKey)
}

// ComputeGenesisRootDirect is the DDF entry point. inputStores MUST be the
// 16 KeyingHashed sub-stores (the layout probe upstream of this call
// rejects a keying skew).
func ComputeGenesisRootDirect(inputStores []*streamsort.Store, tmpDir string) (Result, error) {
	if len(inputStores) != NumInputParts {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRootDirect: got %d input stores, want %d", len(inputStores), NumInputParts)
	}

	// sinks starts non-nil: a leaf-only trie (zero ≥2-child branch nodes —
	// e.g. a single account) registers no sink at all, and Result must still
	// present the DDF mode ("empty branch set") rather than "closed".
	reg := &directSinks{tmpDir: tmpDir, sinks: make([]*streamsort.Store, 0, NumInputParts+1)}
	handedOff := false
	defer func() {
		if !handedOff {
			for _, s := range reg.sinks {
				s.Close()
			}
		}
	}()

	var streams [16]hph.KeyStream
	cursors := make([]*streamsort.Cursor, 0, NumInputParts)
	defer func() {
		for _, c := range cursors {
			_ = c.Close()
		}
	}()
	for i := range inputStores {
		cur, err := inputStores[i].NewCursor()
		if err != nil {
			return Result{}, fmt.Errorf("commitment.ComputeGenesisRootDirect: cursor[%d]: %w", i, err)
		}
		cursors = append(cursors, cur)
		streams[i] = &cursorStream{cur: cur}
	}

	factory := func() (hph.PatriciaContext, func()) {
		c := &hphDirectCtx{reg: reg}
		return c, func() {
			var err error
			if c.sink != nil {
				err = c.sink.Finalize()
			}
			reg.register(c.sink, c.rows, err)
			c.sink = nil
		}
	}

	rootCtx := &hphDirectCtx{reg: reg}
	rootHph := hph.NewHexPatriciaHashed(20, rootCtx)
	pph := hph.NewConcurrentPatriciaHashed(rootHph, rootCtx)

	rootBytes, err := hph.DirectFold(context.Background(), pph, factory, &streams)
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRootDirect: DirectFold: %w", err)
	}

	// Flush the root HPH's deferred branch rows (the root row, prefix 0x00)
	// into the root ctx's sink — same contract as the engine path. Register
	// the sink BEFORE acting on any error so the !handedOff cleanup always
	// covers it.
	applyErr := rootHph.ApplyAndClearInlineDeferredUpdates()
	var finErr error
	if rootCtx.sink != nil {
		finErr = rootCtx.sink.Finalize()
	}
	reg.register(rootCtx.sink, rootCtx.rows, finErr)
	if applyErr != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRootDirect: ApplyAndClearInlineDeferredUpdates: %w", applyErr)
	}
	if err := reg.err(); err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRootDirect: finalize branch sinks: %w", err)
	}

	hphState, err := rootHph.EncodeCurrentState(nil)
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRootDirect: EncodeCurrentState: %w", err)
	}

	var root gethcommon.Hash
	copy(root[:], rootBytes)

	handedOff = true
	return Result{
		Root:        root,
		HPHState:    hphState,
		BranchCount: reg.rows,
		branchSinks: reg.sinks,
	}, nil
}

// mergedBranchIterate streams the union of the sorted sinks in ascending
// prefix order. Worker prefixes are nibble-disjoint and the root row is
// unique, so no duplicate keys arise; a k-way heap merge suffices.
func mergedBranchIterate(sinks []*streamsort.Store, yield func(prefix, data []byte) error) error {
	h := make(cursorHeap, 0, len(sinks))
	defer func() {
		for _, e := range h {
			_ = e.cur.Close()
		}
	}()
	for i, s := range sinks {
		cur, err := s.NewCursor()
		if err != nil {
			return fmt.Errorf("commitment: merge cursor[%d]: %w", i, err)
		}
		if cur.Valid() {
			h = append(h, &cursorHeapEntry{cur: cur})
		} else {
			err := cur.Err()
			_ = cur.Close()
			if err != nil {
				return err
			}
		}
	}
	heap.Init(&h)
	for h.Len() > 0 {
		e := h[0]
		if err := yield(e.cur.Key(), e.cur.Value()); err != nil {
			return err
		}
		e.cur.Next()
		if e.cur.Valid() {
			heap.Fix(&h, 0)
		} else {
			err := e.cur.Err()
			_ = e.cur.Close()
			heap.Pop(&h)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type cursorHeapEntry struct{ cur *streamsort.Cursor }

type cursorHeap []*cursorHeapEntry

func (h cursorHeap) Len() int      { return len(h) }
func (h cursorHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h cursorHeap) Less(i, j int) bool {
	return string(h[i].cur.Key()) < string(h[j].cur.Key())
}
func (h *cursorHeap) Push(x any) { *h = append(*h, x.(*cursorHeapEntry)) }
func (h *cursorHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}
