package recsplit

import "testing"

// TestBucketCollector_PreservesCollisions guards the B2 seq-suffix
// design. Two records sharing the SAME (bucketIdx, fingerprintLo) is
// exactly the collision that flushCurrentBucket must DETECT. The
// collector is now Pebble-backed (streamsort), and Pebble deduplicates
// identical keys — so the 8-byte seq suffix is what keeps both colliding
// records distinct on disk. Without it, only one record survives, the
// collision is silently swallowed, and a structurally wrong .kvi is
// emitted. This test fails (3 records instead of 4) if the suffix is ever
// dropped.
func TestBucketCollector_PreservesCollisions(t *testing.T) {
	c, err := newBucketCollector(t.TempDir())
	if err != nil {
		t.Fatalf("newBucketCollector: %v", err)
	}
	defer c.Close()

	// (bucket 5, fp 0xABCD) appears twice — the collision — plus two
	// distinct records to confirm normal ordering is unaffected.
	adds := []struct {
		bucket uint32
		fp     uint64
		off    uint64
	}{
		{5, 0xABCD, 100},
		{5, 0xABCD, 200}, // collision with the previous record
		{5, 0xABCE, 300},
		{2, 0x1111, 400},
	}
	for i, a := range adds {
		if err := c.Add(a.bucket, a.fp, a.off); err != nil {
			t.Fatalf("Add[%d]: %v", i, err)
		}
	}
	if err := c.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	var got []bucketEntry
	if err := c.ForEach(func(e bucketEntry) error { got = append(got, e); return nil }); err != nil {
		t.Fatalf("ForEach: %v", err)
	}

	if len(got) != len(adds) {
		t.Fatalf("Pebble deduped a colliding record: got %d entries, want %d (seq suffix not defeating dedup)", len(got), len(adds))
	}
	if c.Len() != len(adds) {
		t.Fatalf("Len()=%d, want %d", c.Len(), len(adds))
	}

	// Sorted by (bucketIdx, fingerprintLo); the two (5,0xABCD) records
	// come adjacent in insertion (seq) order, so flushCurrentBucket's
	// consecutive-fingerprint check will fire on them.
	want := []bucketEntry{
		{bucketIdx: 2, fingerprintLo: 0x1111, offset: 400},
		{bucketIdx: 5, fingerprintLo: 0xABCD, offset: 100},
		{bucketIdx: 5, fingerprintLo: 0xABCD, offset: 200},
		{bucketIdx: 5, fingerprintLo: 0xABCE, offset: 300},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
