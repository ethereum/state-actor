package geth

import (
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/ethereum/go-ethereum/ethdb"
)

// pebbleKV is a thin ethdb.KeyValueStore over a *pebble.DB owned externally.
//
// The geth Writer opens cockroachdb/pebble directly so it can drive
// keccak-sorted bulk-import via *pebble.Batch in the hot path. Cold-path
// callers (genesis_block.go, the rawdb.Write* helpers, rawdb.Open's freezer
// init) still want the high-level ethdb.KeyValueStore surface — pebbleKV
// satisfies that without dragging in go-ethereum/ethdb/pebble's metrics,
// stall logger, and write-options machinery.
//
// The adapter does NOT own the *pebble.DB; Close is a no-op. Lifecycle is
// owned by Writer.
type pebbleKV struct {
	db        *pebble.DB
	syncWrite *pebble.WriteOptions // nil → unsynced
}

// noSync is shared (non-syncing) writes for Put/Delete fast paths. The
// Writer issues the final fsync explicitly via SyncKeyValue / Close.
var noSync = &pebble.WriteOptions{Sync: false}

// newPebbleKV returns a kv adapter over an already-open pebble DB. sync
// controls Put/Delete durability; nil means unsynced.
func newPebbleKV(db *pebble.DB, sync *pebble.WriteOptions) *pebbleKV {
	if sync == nil {
		sync = noSync
	}
	return &pebbleKV{db: db, syncWrite: sync}
}

// --- KeyValueReader ---

func (p *pebbleKV) Has(key []byte) (bool, error) {
	val, closer, err := p.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return val != nil, nil
}

func (p *pebbleKV) Get(key []byte) ([]byte, error) {
	val, closer, err := p.db.Get(key)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	out := make([]byte, len(val))
	copy(out, val)
	return out, nil
}

// --- KeyValueWriter ---

func (p *pebbleKV) Put(key, value []byte) error {
	return p.db.Set(key, value, p.syncWrite)
}

func (p *pebbleKV) Delete(key []byte) error {
	return p.db.Delete(key, p.syncWrite)
}

// --- KeyValueRangeDeleter ---

func (p *pebbleKV) DeleteRange(start, end []byte) error {
	return p.db.DeleteRange(start, end, p.syncWrite)
}

// --- KeyValueStater ---

func (p *pebbleKV) Stat() (string, error) {
	return p.db.Metrics().String(), nil
}

// --- KeyValueSyncer ---

func (p *pebbleKV) SyncKeyValue() error {
	// Pebble's LogData(nil, Sync) flushes the WAL — but we skip the WAL
	// entirely for bulk import. A no-op here is acceptable because Writer
	// commits each batch with its own *WriteOptions.Sync at flush time.
	return nil
}

// --- Compacter ---

func (p *pebbleKV) Compact(start, end []byte) error {
	if start == nil {
		start = []byte{}
	}
	if end == nil {
		// Pebble requires a finite upper bound; 0xff... is the sentinel.
		end = bytes32xFF
	}
	return p.db.Compact(start, end, true)
}

// bytes32xFF is the Pebble-friendly "key after all keys" sentinel. Pebble's
// Compact / DeleteRange APIs require a non-nil upper bound; 32 bytes of
// 0xFF is larger than any 32-byte hash key the geth schema uses.
var bytes32xFF = []byte{
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

// --- Batcher ---

func (p *pebbleKV) NewBatch() ethdb.Batch {
	return &pebbleBatch{
		db:    p.db,
		batch: p.db.NewBatch(),
		sync:  p.syncWrite,
	}
}

func (p *pebbleKV) NewBatchWithSize(size int) ethdb.Batch {
	return &pebbleBatch{
		db:    p.db,
		batch: p.db.NewBatchWithSize(size),
		sync:  p.syncWrite,
	}
}

// --- Iteratee ---

func (p *pebbleKV) NewIterator(prefix, start []byte) ethdb.Iterator {
	// ethdb's contract: prefix scopes the keyspace, start is appended to
	// prefix to position the cursor. Pebble wants a [LowerBound, UpperBound)
	// range; we synthesize that from prefix.
	lower := append(append([]byte{}, prefix...), start...)
	var upper []byte
	if len(prefix) > 0 {
		upper = upperBoundForPrefix(prefix)
	}
	iter, err := p.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		// Pebble returns errors only for closed DBs / config errors; surface
		// via a stub iterator that always reports the error.
		return &pebbleIter{err: err}
	}
	return &pebbleIter{iter: iter}
}

// upperBoundForPrefix returns the smallest key strictly greater than every
// key with the given prefix. Increments the last non-0xFF byte; if all
// bytes are 0xFF, returns nil (= unbounded above).
func upperBoundForPrefix(prefix []byte) []byte {
	end := append([]byte{}, prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// --- io.Closer ---
//
// Close is a no-op. The *pebble.DB lifecycle is owned by Writer, which
// closes it directly via Writer.Close. Calling Close on the adapter does
// not propagate so that ad-hoc rawdb wrappers (rawdb.Open's freezer) can
// fdb.Close() without invalidating the underlying handle.
func (p *pebbleKV) Close() error {
	return nil
}

// --- pebbleBatch ---

// pebbleBatch satisfies ethdb.Batch over a *pebble.Batch. ValueSize tracks
// queued bytes; pebble's own batch.Len() includes header overhead which
// would mislead callers that drive flush thresholds.
type pebbleBatch struct {
	db    *pebble.DB
	batch *pebble.Batch
	sync  *pebble.WriteOptions
	size  int
}

func (b *pebbleBatch) Put(key, value []byte) error {
	b.size += len(key) + len(value)
	return b.batch.Set(key, value, nil)
}

func (b *pebbleBatch) Delete(key []byte) error {
	b.size += len(key)
	return b.batch.Delete(key, nil)
}

func (b *pebbleBatch) DeleteRange(start, end []byte) error {
	b.size += len(start) + len(end)
	return b.batch.DeleteRange(start, end, nil)
}

func (b *pebbleBatch) ValueSize() int { return b.size }

func (b *pebbleBatch) Write() error {
	if err := b.batch.Commit(b.sync); err != nil {
		return fmt.Errorf("pebble batch commit: %w", err)
	}
	return nil
}

func (b *pebbleBatch) Reset() {
	b.batch.Reset()
	b.size = 0
}

func (b *pebbleBatch) Replay(w ethdb.KeyValueWriter) error {
	reader := b.batch.Reader()
	for {
		kind, key, value, ok, err := reader.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		switch kind {
		case pebble.InternalKeyKindSet:
			if err := w.Put(key, value); err != nil {
				return err
			}
		case pebble.InternalKeyKindDelete:
			if err := w.Delete(key); err != nil {
				return err
			}
		case pebble.InternalKeyKindRangeDelete:
			// ethdb.KeyValueWriter has no DeleteRange; widen to a typed
			// caller if available, else error out loud — silent skip would
			// drop user intent.
			if rd, ok := w.(ethdb.KeyValueRangeDeleter); ok {
				if err := rd.DeleteRange(key, value); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("pebbleBatch.Replay: target writer does not support DeleteRange")
			}
		default:
			// MERGE, RANGEKEY*, etc. — geth doesn't issue these, fail loud
			// rather than silently swallow.
			return fmt.Errorf("pebbleBatch.Replay: unsupported batch entry kind %v", kind)
		}
	}
}

func (b *pebbleBatch) Close() {
	_ = b.batch.Close()
}

// --- pebbleIter ---

type pebbleIter struct {
	iter    *pebble.Iterator
	started bool // false until first Next() positions to First()
	err     error
}

// Next advances the cursor and reports whether a valid key/value pair is
// available. Implements the ethdb.Iterator contract: the first Next() call
// positions to the first matching key; subsequent calls advance.
func (i *pebbleIter) Next() bool {
	if i.err != nil || i.iter == nil {
		return false
	}
	if !i.started {
		i.started = true
		return i.iter.First()
	}
	return i.iter.Next()
}

func (i *pebbleIter) Error() error {
	if i.err != nil {
		return i.err
	}
	if i.iter == nil {
		return nil
	}
	return i.iter.Error()
}

func (i *pebbleIter) Key() []byte {
	if i.iter == nil || !i.iter.Valid() {
		return nil
	}
	k := i.iter.Key()
	out := make([]byte, len(k))
	copy(out, k)
	return out
}

func (i *pebbleIter) Value() []byte {
	if i.iter == nil || !i.iter.Valid() {
		return nil
	}
	v := i.iter.Value()
	out := make([]byte, len(v))
	copy(out, v)
	return out
}

func (i *pebbleIter) Release() {
	if i.iter != nil {
		_ = i.iter.Close()
		i.iter = nil
	}
}
