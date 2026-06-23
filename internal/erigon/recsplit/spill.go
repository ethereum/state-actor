package recsplit

import (
	"encoding/binary"
	"sort"
)

// bucketEntry is one record collected per AddKey call: the bucket index
// (uint32 BE in Erigon's wire layout), the fingerprint lo64 within the
// bucket, and the offset value the perfect-hash function must resolve
// to.
//
// Wire-equivalent to Erigon's `bucketKeyBuf = bucketIdx (4 BE) ||
// fingerprintLo (8 BE)`. We keep separate fields so the sort comparator
// can index without re-parsing.
type bucketEntry struct {
	bucketIdx     uint32
	fingerprintLo uint64
	offset        uint64
}

// bucketCollector is the in-memory replacement for Erigon's
// etl.Collector. For the spike scope (≤ 1M keys) we don't need
// disk-spilling; a simple slice + sort is enough and avoids the etl
// import chain (which pulls log/v3, dir, mmap, ...).
//
// CRITICAL: the sort comparator MUST match Erigon's etl.Collector byte
// order — Erigon sorts `bucketKeyBuf = bucketIdx (4 BE) ||
// fingerprintLo (8 BE)`. Lexicographic on BE bytes is identical to
// `(bucketIdx, fingerprintLo)` int comparison since both fields are
// fixed-width and non-negative.
type bucketCollector struct {
	entries []bucketEntry
}

func newBucketCollector(hintCap int) *bucketCollector {
	return &bucketCollector{entries: make([]bucketEntry, 0, hintCap)}
}

// Add records one (bucketIdx, fingerprintLo, offset) triple.
func (c *bucketCollector) Add(bucketIdx uint32, fingerprintLo, offset uint64) {
	c.entries = append(c.entries, bucketEntry{
		bucketIdx:     bucketIdx,
		fingerprintLo: fingerprintLo,
		offset:        offset,
	})
}

// Reset clears the buffer for a salt-bump retry. Capacity is preserved.
func (c *bucketCollector) Reset() {
	c.entries = c.entries[:0]
}

// Len returns the current entry count.
func (c *bucketCollector) Len() int { return len(c.entries) }

// SortByBucketThenFingerprint sorts entries to match Erigon's
// etl.Collector output order: lexicographic on `bucketIdx (4B BE) ||
// fingerprintLo (8B BE)`.
func (c *bucketCollector) SortByBucketThenFingerprint() {
	sort.Slice(c.entries, func(i, j int) bool {
		a, b := &c.entries[i], &c.entries[j]
		if a.bucketIdx != b.bucketIdx {
			return a.bucketIdx < b.bucketIdx
		}
		return a.fingerprintLo < b.fingerprintLo
	})
}

// Iter yields each entry in sort order. Must be called after Sort.
func (c *bucketCollector) Iter() []bucketEntry { return c.entries }

// EncodeKey serializes (bucketIdx, fingerprintLo) the way Erigon's
// bucketKeyBuf does. Exported for fuzz / parity tests against Erigon's
// own etl-collected output.
func EncodeKey(bucketIdx uint32, fingerprintLo uint64) []byte {
	var buf [12]byte
	binary.BigEndian.PutUint32(buf[:4], bucketIdx)
	binary.BigEndian.PutUint64(buf[4:], fingerprintLo)
	return buf[:]
}
