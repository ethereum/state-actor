package recsplit

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/state-actor/internal/streamsort"
)

// bucketEntry is one record collected per AddKey call: the bucket index
// (uint32 BE in Erigon's wire layout), the fingerprint lo64 within the
// bucket, and the offset value the perfect-hash function must resolve
// to.
//
// Wire-equivalent to Erigon's `bucketKeyBuf = bucketIdx (4 BE) ||
// fingerprintLo (8 BE)`. We keep separate fields so the bucket walk can
// index without re-parsing.
type bucketEntry struct {
	bucketIdx     uint32
	fingerprintLo uint64
	offset        uint64
}

// bucketSpillKeyLen is the spill-key width: bucketIdx(4) ||
// fingerprintLo(8) || seq(8). See bucketCollector for why seq is here.
const bucketSpillKeyLen = 4 + 8 + 8

// bucketCollector is a disk-backed external sort over bucketEntry
// (streamsort/Pebble: Add → Put, ForEach → sorted Iterate) — peak RAM is
// the memtable, independent of key count. Key layout:
//
//	bucketIdx (4 BE) || fingerprintLo (8 BE) || seq (8 BE)   value = offset (8 BE)
//
// The `seq` suffix is LOAD-BEARING: Pebble dedups identical keys, so
// without it a (bucketIdx, fingerprintLo) collision — exactly what the
// bucket walk must DETECT — would be silently swallowed. On a
// collision-free build the 12-byte prefix fully orders the records, so
// the output is byte-identical to the old in-memory sort.
type bucketCollector struct {
	tmpDir string
	store  *streamsort.Store
	seq    uint64
	n      int
}

// spillOpts: sequential spill drained by one ForEach — 64 MiB arenas
// (off-heap C malloc) instead of the 256 MiB default.
var spillOpts = streamsort.Options{MemTableBytes: 64 << 20}

// newBucketCollector opens a fresh streamsort under tmpDir (empty →
// os.TempDir()). Returns an error if the backing store can't be created.
func newBucketCollector(tmpDir string) (*bucketCollector, error) {
	store, err := streamsort.NewWithOptions(tmpDir, spillOpts)
	if err != nil {
		return nil, fmt.Errorf("recsplit: open bucket streamsort: %w", err)
	}
	return &bucketCollector{tmpDir: tmpDir, store: store}, nil
}

// Add spills one (bucketIdx, fingerprintLo, offset) record.
func (c *bucketCollector) Add(bucketIdx uint32, fingerprintLo, offset uint64) error {
	var key [bucketSpillKeyLen]byte
	binary.BigEndian.PutUint32(key[0:4], bucketIdx)
	binary.BigEndian.PutUint64(key[4:12], fingerprintLo)
	binary.BigEndian.PutUint64(key[12:20], c.seq)
	c.seq++
	var val [8]byte
	binary.BigEndian.PutUint64(val[:], offset)
	if err := c.store.Put(key[:], val[:]); err != nil {
		return fmt.Errorf("recsplit: bucket spill Put: %w", err)
	}
	c.n++
	return nil
}

// Finalize seals the store for reading. streamsort is already sorted by
// key bytes, so this is the disk-backed replacement for the old in-memory
// SortByBucketThenFingerprint.
func (c *bucketCollector) Finalize() error { return c.store.Finalize() }

// ForEach yields each entry in (bucketIdx, fingerprintLo) order. Must be
// called after Finalize.
func (c *bucketCollector) ForEach(fn func(bucketEntry) error) error {
	return c.store.Iterate(func(k, v []byte) error {
		return fn(bucketEntry{
			bucketIdx:     binary.BigEndian.Uint32(k[0:4]),
			fingerprintLo: binary.BigEndian.Uint64(k[4:12]),
			offset:        binary.BigEndian.Uint64(v),
		})
	})
}

// Reset discards the spilled records for a salt-bump retry by closing the
// old store (which removes its temp dir) and opening a fresh one.
func (c *bucketCollector) Reset() error {
	if c.store != nil {
		_ = c.store.Close()
	}
	store, err := streamsort.NewWithOptions(c.tmpDir, spillOpts)
	if err != nil {
		return fmt.Errorf("recsplit: reopen bucket streamsort: %w", err)
	}
	c.store = store
	c.seq = 0
	c.n = 0
	return nil
}

// Len returns the number of spilled records.
func (c *bucketCollector) Len() int { return c.n }

// Close releases the backing store and removes its temp dir. Idempotent.
func (c *bucketCollector) Close() error {
	if c.store == nil {
		return nil
	}
	err := c.store.Close()
	c.store = nil
	return err
}

// EncodeKey serializes (bucketIdx, fingerprintLo) the way Erigon's
// bucketKeyBuf does. Exported for fuzz / parity tests against Erigon's
// own etl-collected output. (The internal spill key adds an 8-byte seq
// suffix on top of this — see bucketCollector — but the wire/sort order
// over distinct keys is identical.)
func EncodeKey(bucketIdx uint32, fingerprintLo uint64) []byte {
	var buf [12]byte
	binary.BigEndian.PutUint32(buf[:4], bucketIdx)
	binary.BigEndian.PutUint64(buf[4:], fingerprintLo)
	return buf[:]
}
