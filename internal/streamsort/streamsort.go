// Package streamsort is a Pebble-backed sorted-spill store with an
// explicit WRITING → FINALIZED → CLOSED state machine.
//
// Lifecycle:
//
//	store, _ := New(dir)
//	defer store.Close()              // safe in any state
//	for ... { store.Put(k, v) }      // WRITING — single-goroutine writer
//	store.Finalize()                 // flushes the batch (optional: a read auto-finalizes)
//	store.Get(k) / store.Iterate(...) // FINALIZED — concurrent-safe readers
//
// Concurrency contract:
//   - Put is single-writer (one goroutine owns the write phase). Multiple
//     concurrent Puts are serialized under putMu but the package contract
//     expects callers to keep Put on a single goroutine.
//   - Put after Finalize returns an error (does not panic).
//   - Get and Iterate auto-finalize on first read when Finalize was not
//     called explicitly: a single-phase caller (fill, then read) can skip
//     it. Concurrent producers MUST Finalize before any reader starts.
//   - After Finalize, Get and Iterate are safe from any number of
//     concurrent goroutines. The pre-read batch flush happens exactly
//     once inside Finalize; subsequent reads go straight to
//     pebble.DB.Get / pebble.DB.NewIter, both documented as thread-safe
//     in Pebble v1.1.5 (db.go:519-600 for Get, iterator.go:177-178 for
//     concurrent NewIter — each returned *Iterator stays on one
//     goroutine).
//   - Close waits for all in-flight Get/Iterate to drain (Pebble forbids
//     Close concurrent with any other DB method per db.go:1557). The
//     readers WaitGroup tracks active readers.
//   - Close is idempotent and safe to call from any state.
//
// The package exists because the natural Pebble idiom (one batched
// write phase then concurrent reads) doesn't compose cleanly without
// an explicit transition — a shared *pebble.Batch is NOT safe for
// concurrent commit (batch.committing is a non-atomic bool with an
// explicit "violations may cause memory safety issues" comment at
// batch.go:305-312). Finalize is the transition that lets us reuse
// the batched fast path for writes while opening up Pebble's
// thread-safe read path for parallel HPH commitment walks.
package streamsort

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/sstable"
)

// MemTableSize is the per-Pebble-MemTable write buffer. Sized for 8
// concurrent streamsort.Stores; peak per-Store RAM (memtable + cache) stays
// under ~520 MiB.
const MemTableSize = 256 << 20

const (
	batchFlushBytes = 64 << 20
	// defaultBlockCacheBytes is small because the default workload is a
	// single sequential scan (Iterate). Callers with random-access read
	// workloads (e.g. ConcurrentPatriciaHashed touching a multi-GiB
	// commitmentInputStore via subtreeCtx.Get on every leaf) should pass
	// a larger value via NewWithOptions — see comment on Options.BlockCacheBytes.
	defaultBlockCacheBytes = 8 << 20
)

// Options is the optional configuration for NewWithOptions. Zero-value
// fields fall back to package defaults.
type Options struct {
	// BlockCacheBytes overrides the Pebble block cache size. Pebble's
	// block cache is the primary in-memory hot-data store for random
	// reads; for read-heavy workloads against multi-GiB Stores the
	// default 8 MiB cache yields very low hit rates and the LSM SST
	// disk reads dominate wall time. Set this to a value sized to the
	// expected working set (e.g. 1-4 GiB for a 12-25 GiB store under
	// HPH commitment walks). Default: 8 MiB.
	//
	// Tuning guidance from upstream Pebble: the cache holds compressed
	// data blocks (~64 KiB each by default). A 4 GiB cache holds ~65k
	// blocks. For an HPH walk that touches every entry once in
	// keccak-sorted order, expect ~30-50% hit rate at this size against
	// a 12 GiB store — enough to drop most cold disk reads.
	BlockCacheBytes int64
}

// Store is a sorted-by-key spill buffer backed by a temp Pebble LSM with
// an explicit WRITING → FINALIZED → CLOSED state machine. See package
// doc for the concurrency contract.
type Store struct {
	dir   string
	cache *pebble.Cache
	db    *pebble.DB
	batch *pebble.Batch

	// putMu serializes Put with the Finalize transition. The size-
	// triggered mid-Put batch.Commit + Reset also runs under it, so
	// a concurrent reader's Finalize cannot race with a Put-time
	// flush (both go through the same lock).
	putMu sync.Mutex

	// finalized is the read gate. Atomic so Get/Iterate hot path is a
	// single sub-ns load — no mutex acquisition during steady-state
	// concurrent reads. Set to true inside finalizeOnce.Do AFTER the
	// batch is flushed; Go memory model guarantees readers that
	// observe finalized=true also see the flushed memtable state.
	finalized atomic.Bool

	finalizeOnce sync.Once
	finalizeErr  error

	// readers tracks in-flight Get/Iterate goroutines. Close waits on
	// it before calling pebble.DB.Close, since Pebble forbids Close
	// concurrent with any other DB method (db.go:1557).
	readers sync.WaitGroup

	// closed is atomic so the read-path gate is also lock-free.
	closed atomic.Bool
}

// New creates a Store rooted under workDir (empty → os.TempDir()) with
// default options. See NewWithOptions for per-Store tuning.
func New(workDir string) (*Store, error) {
	return NewWithOptions(workDir, Options{})
}

// NewWithOptions creates a Store rooted under workDir with the supplied
// options. Zero-value fields fall back to package defaults. Caller is
// responsible for sufficient free disk to hold the spilled dataset.
func NewWithOptions(workDir string, opts Options) (*Store, error) {
	dir, err := os.MkdirTemp(workDir, "streamsort-*")
	if err != nil {
		return nil, fmt.Errorf("streamsort: mkdir temp: %w", err)
	}

	cacheSize := int64(defaultBlockCacheBytes)
	if opts.BlockCacheBytes > 0 {
		cacheSize = opts.BlockCacheBytes
	}
	cache := pebble.NewCache(cacheSize)
	pebbleOpts := &pebble.Options{
		DisableWAL:                  true,
		MemTableSize:                MemTableSize,
		MemTableStopWritesThreshold: 2,
		L0CompactionThreshold:       math.MaxInt32,
		L0StopWritesThreshold:       math.MaxInt32,
		// Cap compaction concurrency at 8. runtime.NumCPU() on a high-core box
		// spawns one compactor goroutine per core, each with transient
		// per-compaction buffers — GiBs of extra RAM during flush/compact that
		// scales with core count. This is a secondary memory contributor,
		// consistent with reducing --cores easing the Besu generation OOM,
		// though the dominant cause there was the non-streaming account trie.
		// 8 keeps compaction off the write path without the per-core blow-up.
		// Mirrors the cap on geth's production Pebble (client/geth/writer.go:116-124).
		MaxConcurrentCompactions: func() int { return min(runtime.NumCPU(), 8) },
		BytesPerSync:             0,
		WALBytesPerSync:          0,
		NoSyncOnClose:            true,
		FormatMajorVersion:       pebble.FormatNewest,
		Cache:                    cache,
		Levels: []pebble.LevelOptions{
			{Compression: sstable.NoCompression},
		},
	}

	db, err := pebble.Open(dir, pebbleOpts)
	if err != nil {
		cache.Unref()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("streamsort: open pebble at %s: %w", dir, err)
	}
	return &Store{
		dir:   dir,
		cache: cache,
		db:    db,
		batch: db.NewBatch(),
	}, nil
}

// Put inserts (key, value) into the pending batch, flushing when the
// batch exceeds batchFlushBytes. Key and value are copied; the caller
// may reuse the input slices.
//
// Errors:
//   - "streamsort: Put after Close" if the store is closed.
//   - "streamsort: Put after Finalize" if Finalize has been called.
//
// The contract assumes a single writer goroutine; putMu is held for
// the entire operation to make the Put / Finalize transition safe but
// the package doc requires callers to keep Put on one goroutine for
// throughput.
func (s *Store) Put(key, value []byte) error {
	if s.closed.Load() {
		return fmt.Errorf("streamsort: Put after Close")
	}
	s.putMu.Lock()
	defer s.putMu.Unlock()
	// Re-check under the lock: Finalize may have flipped finalized
	// between our pre-lock check (none above) and now.
	if s.finalized.Load() {
		return fmt.Errorf("streamsort: Put after Finalize")
	}
	if err := s.batch.Set(key, value, nil); err != nil {
		return fmt.Errorf("streamsort: batch.Set: %w", err)
	}
	if s.batch.Len() >= batchFlushBytes {
		if err := s.batch.Commit(pebble.NoSync); err != nil {
			return fmt.Errorf("streamsort: batch.Commit: %w", err)
		}
		s.batch.Reset()
	}
	return nil
}

// Finalize flushes the pending batch and marks the store read-only.
// Idempotent and safe to call from any goroutine — subsequent calls
// return the cached result of the first.
//
// After a successful Finalize, Put returns an error and Get/Iterate
// become safe for concurrent callers (Pebble's read path is
// thread-safe; the batch is no longer involved).
//
// Errors: only the batch.Commit error from the underlying Pebble call
// (cached for subsequent Finalize callers).
func (s *Store) Finalize() error {
	if s.closed.Load() {
		return fmt.Errorf("streamsort: Finalize after Close")
	}
	s.finalizeOnce.Do(func() {
		// Hold putMu so any in-flight Put completes before we flush
		// the batch — its size-triggered mid-Put Commit would race
		// with our flush otherwise.
		s.putMu.Lock()
		defer s.putMu.Unlock()
		if err := s.batch.Commit(pebble.NoSync); err != nil {
			s.finalizeErr = fmt.Errorf("streamsort: Finalize batch.Commit: %w", err)
			return
		}
		s.batch.Reset()
		// Set finalized AFTER the flush completes — readers that
		// observe finalized=true also observe the flushed memtable.
		s.finalized.Store(true)
	})
	return s.finalizeErr
}

// Get returns the value associated with key, or (nil, nil) if the key
// is absent. The returned slice is COPIED out of Pebble's buffer —
// safe to retain past subsequent calls.
//
// Errors:
//   - "streamsort: Get after Close" if the store is closed.
//
// Auto-finalizes on first read if Finalize was not called explicitly.
// After finalization, safe for any number of concurrent callers.
func (s *Store) Get(key []byte) ([]byte, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("streamsort: Get after Close")
	}
	// Auto-finalize on first read (see Iterate for the concurrency note).
	if !s.finalized.Load() {
		if err := s.Finalize(); err != nil {
			return nil, err
		}
	}
	s.readers.Add(1)
	defer s.readers.Done()

	val, closer, err := s.db.Get(key)
	if err != nil {
		if err == pebble.ErrNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("streamsort: db.Get: %w", err)
	}
	defer func() { _ = closer.Close() }()
	out := make([]byte, len(val))
	copy(out, val)
	return out, nil
}

// Iterate invokes yield for every entry in ascending key order. Each
// call gets its own pebble.DB iterator — Pebble's NewIter is safe for
// concurrent callers (iterator.go:177-178: "An iterator is not
// goroutine-safe, but it is safe to use multiple iterators
// concurrently, with each in a dedicated goroutine"). Key/value slices
// alias Pebble's internal buffers and are invalidated on the next
// Next(); callers that retain either MUST copy it.
//
// Errors:
//   - "streamsort: Iterate after Close" if the store is closed.
//   - any non-nil error returned by yield short-circuits and is returned.
//
// Auto-finalizes on first read if Finalize was not called explicitly.
// After finalization, safe for any number of concurrent callers.
func (s *Store) Iterate(yield func(key, value []byte) error) error {
	if s.closed.Load() {
		return fmt.Errorf("streamsort: Iterate after Close")
	}
	// Auto-finalize on first read. A single-phase caller (fill the store,
	// then read it back) need not call Finalize explicitly. Concurrent
	// producers MUST still Finalize explicitly before any reader starts —
	// otherwise a read could seal the store mid-production. erigon's
	// parallel-HPH path does exactly that. Finalize is idempotent and
	// goroutine-safe, so this is a no-op when already finalized.
	if !s.finalized.Load() {
		if err := s.Finalize(); err != nil {
			return err
		}
	}
	s.readers.Add(1)
	defer s.readers.Done()

	iter, err := s.db.NewIter(nil)
	if err != nil {
		return fmt.Errorf("streamsort: NewIter: %w", err)
	}
	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		if err := yield(iter.Key(), iter.Value()); err != nil {
			return err
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("streamsort: iter.Error: %w", err)
	}
	return nil
}

// Close flushes any pending batch (if Finalize was not called), waits
// for in-flight readers to drain, closes the DB, frees the cache, and
// removes the temp directory. Idempotent. RemoveAll failures are
// logged, not returned.
//
// The reader-drain is required because Pebble forbids DB.Close
// concurrent with any other DB method (db.go:1557).
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}

	var firstErr error
	// Cover the Put-only-never-read lifecycle: flush whatever's in the
	// batch under putMu. If Finalize was already called the batch is
	// already empty; the Commit is a no-op but harmless.
	s.putMu.Lock()
	if !s.finalized.Load() {
		if err := s.batch.Commit(pebble.NoSync); err != nil {
			firstErr = fmt.Errorf("streamsort: final batch.Commit: %w", err)
		}
		s.batch.Reset()
	}
	s.putMu.Unlock()

	// Wait for any in-flight Get/Iterate to complete before closing
	// the Pebble DB. New Get/Iterate calls see closed=true and bail
	// before incrementing readers (see Swap above).
	s.readers.Wait()

	if err := s.db.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("streamsort: db.Close: %w", err)
	}
	if s.cache != nil {
		s.cache.Unref()
	}
	if err := os.RemoveAll(s.dir); err != nil {
		log.Printf("streamsort: cleanup of %s failed: %v", s.dir, err)
	}
	return firstErr
}
