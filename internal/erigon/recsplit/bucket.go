package recsplit

import (
	"encoding/binary"
	"fmt"
)

// recsplitScratch holds per-bucket scratch buffers + config shared
// between findBijection/findSplit/recsplit. Port of the same-named
// Erigon struct (recsplit.go:88-106) without the parallel-path fields.
type recsplitScratch struct {
	count              []uint16 // size = 8 * secondaryAggrBound (8-way salt parallelism in findSplit)
	buffer             []uint64
	offsetBuffer       []uint64
	numBuf             [8]byte
	startSeed          []uint64
	leafSize           uint16
	primaryAggrBound   uint16
	secondaryAggrBound uint16
	bytesPerRec        int
	golombParams       *golombParamCache
}

// preAlloc ensures buffer/offsetBuffer have at least n slots.
func (sc *recsplitScratch) preAlloc(n int) {
	if cap(sc.buffer) < n {
		sc.buffer = make([]uint64, n)
	} else {
		sc.buffer = sc.buffer[:n]
	}
	if cap(sc.offsetBuffer) < n {
		sc.offsetBuffer = make([]uint64, n)
	} else {
		sc.offsetBuffer = sc.offsetBuffer[:n]
	}
}

// bucketResult collects the per-bucket recsplit() output: serialized
// leaf-offset bytes + a per-bucket GolombRice fixed-length stream, plus
// the routing fields the parallel Build consumer needs to write results
// in the exact sequential order (seq is the dense dispatch number —
// bucket indices are sparse when buckets are empty).
type bucketResult struct {
	offsetData []byte
	gr         GolombRice
	unary      []uint64
	bucketIdx  uint64
	seq        uint64
	bucketSize int
}

// findBijection finds a salt s ≥ startSalt such that each fingerprint in
// `bucket` (assumed size m ≤ leafSize ≤ MaxLeafSize) is mapped to a
// DISTINCT slot in [0, m) via `remap16(remix(key+s), m)`. Returns the
// successful salt.
//
// Port of recsplit.go:725-770 with the same 8-way salt parallelism (try
// salt, salt+1, ..., salt+7 simultaneously and pick the first that
// produces no collisions).
//
// Termination: when m ≤ MaxLeafSize, the probability that a random salt
// yields a bijection is ~m! / m^m ≈ √(2πm)·e^-m (Stirling). For m=8,
// that's about 1/415. Erigon's startSeed table is sized for this.
func findBijection(bucket []uint64, salt uint64) uint64 {
	m := uint16(len(bucket))
	fullMask := uint32((1 << m) - 1)
	for {
		var mask0, mask1, mask2, mask3, mask4, mask5, mask6, mask7 uint32
		for i := uint16(0); i < m; i++ {
			key := bucket[i]
			// `& 31` is a no-op for m ≤ 24 but tells the compiler the
			// shift can't overflow (matches Erigon's recsplit.go:735).
			mask0 |= uint32(1) << remap16(remix(key+salt), m&31)
			mask1 |= uint32(1) << remap16(remix(key+salt+1), m&31)
			mask2 |= uint32(1) << remap16(remix(key+salt+2), m&31)
			mask3 |= uint32(1) << remap16(remix(key+salt+3), m&31)
			mask4 |= uint32(1) << remap16(remix(key+salt+4), m&31)
			mask5 |= uint32(1) << remap16(remix(key+salt+5), m&31)
			mask6 |= uint32(1) << remap16(remix(key+salt+6), m&31)
			mask7 |= uint32(1) << remap16(remix(key+salt+7), m&31)
		}
		if mask0 == fullMask {
			return salt
		}
		if mask1 == fullMask {
			return salt + 1
		}
		if mask2 == fullMask {
			return salt + 2
		}
		if mask3 == fullMask {
			return salt + 3
		}
		if mask4 == fullMask {
			return salt + 4
		}
		if mask5 == fullMask {
			return salt + 5
		}
		if mask6 == fullMask {
			return salt + 6
		}
		if mask7 == fullMask {
			return salt + 7
		}
		salt += 8
	}
}

// findSplit finds a salt s ≥ startSalt that splits `bucket` (size m) into
// `fanout` partitions of `unit` keys each (except possibly the last,
// which holds m - (fanout-1)*unit). `count` must have at least 8*fanout
// slots — it's carved into eight independent count arrays for 8-way
// salt parallelism.
//
// Port of recsplit.go:657-719. Branchless OR-accumulate validation: for
// each candidate salt we compute the partition counts c[i], then XOR
// each c[i] with unit and OR them — bad==0 iff every partition has
// exactly `unit` keys (note: the last partition is excluded from the
// check by `i < fanout-1` because its size depends on m mod unit).
func findSplit(bucket []uint64, salt uint64, fanout, unit uint16, count []uint16) uint64 {
	m := uint16(len(bucket))
	c0 := count[0*fanout : 1*fanout : 1*fanout]
	c1 := count[1*fanout : 2*fanout : 2*fanout]
	c2 := count[2*fanout : 3*fanout : 3*fanout]
	c3 := count[3*fanout : 4*fanout : 4*fanout]
	c4 := count[4*fanout : 5*fanout : 5*fanout]
	c5 := count[5*fanout : 6*fanout : 6*fanout]
	c6 := count[6*fanout : 7*fanout : 7*fanout]
	c7 := count[7*fanout : 8*fanout : 8*fanout]
	for {
		clear(count[:8*fanout])
		for i := uint16(0); i < m; i++ {
			key := bucket[i]
			c0[remap16(remix(key+salt), m)/unit]++
			c1[remap16(remix(key+salt+1), m)/unit]++
			c2[remap16(remix(key+salt+2), m)/unit]++
			c3[remap16(remix(key+salt+3), m)/unit]++
			c4[remap16(remix(key+salt+4), m)/unit]++
			c5[remap16(remix(key+salt+5), m)/unit]++
			c6[remap16(remix(key+salt+6), m)/unit]++
			c7[remap16(remix(key+salt+7), m)/unit]++
		}
		var bad0, bad1, bad2, bad3, bad4, bad5, bad6, bad7 uint16
		for i := uint16(0); i < fanout-1; i++ {
			bad0 |= c0[i] ^ unit
			bad1 |= c1[i] ^ unit
			bad2 |= c2[i] ^ unit
			bad3 |= c3[i] ^ unit
			bad4 |= c4[i] ^ unit
			bad5 |= c5[i] ^ unit
			bad6 |= c6[i] ^ unit
			bad7 |= c7[i] ^ unit
		}
		if bad0 == 0 {
			return salt
		}
		if bad1 == 0 {
			return salt + 1
		}
		if bad2 == 0 {
			return salt + 2
		}
		if bad3 == 0 {
			return salt + 3
		}
		if bad4 == 0 {
			return salt + 4
		}
		if bad5 == 0 {
			return salt + 5
		}
		if bad6 == 0 {
			return salt + 6
		}
		if bad7 == 0 {
			return salt + 7
		}
		salt += 8
	}
}

// recsplit applies the recursive RecSplit algorithm to one bucket.
//
// Inputs:
//   - level:   recursion depth (selects startSeed[level])
//   - bucket:  fingerprints in this sub-bucket (MUTATED in place during split)
//   - offsets: parallel offsets for each fingerprint (MUTATED in place)
//   - unary:   in-place accumulator of unary high-bit codes for this bucket
//   - sc:      scratch + golomb params
//   - result:  per-bucket result accumulator (offsetData + fixed GolombRice)
//
// Output: the (possibly grown) unary slice.
//
// Port of recsplit.go:774-838.
func recsplitRecurse(level int, bucket []uint64, offsets []uint64, unary []uint64, sc *recsplitScratch, result *bucketResult) ([]uint64, error) {
	salt := sc.startSeed[level]
	m := uint16(len(bucket))
	if m <= sc.leafSize {
		// Base case: find a bijection, emit offsets in the bijection-induced order.
		salt = findBijection(bucket, salt)
		for i := uint16(0); i < m; i++ {
			j := remap16(remix(bucket[i]+salt), m)
			sc.offsetBuffer[j] = offsets[i]
		}
		for _, off := range sc.offsetBuffer[:m] {
			binary.BigEndian.PutUint64(sc.numBuf[:], off)
			result.offsetData = append(result.offsetData, sc.numBuf[8-sc.bytesPerRec:]...)
		}
		salt -= sc.startSeed[level]
		log2g := sc.golombParams.param(m)
		result.gr.appendFixed(salt, log2g)
		unary = append(unary, salt>>log2g)
		return unary, nil
	}

	// Recursive case: find a salt that splits the bucket into `fanout`
	// partitions of `unit` keys each; permute bucket/offsets in place;
	// emit the salt's fixed bits + unary; recurse.
	fanout, unit := splitParams(m, sc.leafSize, sc.primaryAggrBound, sc.secondaryAggrBound)
	count := sc.count
	if int(8*fanout) > len(count) {
		return nil, fmt.Errorf("recsplit: count buffer too small (%d < 8*%d) — secondaryAggrBound miscalc?", len(count), fanout)
	}
	salt = findSplit(bucket, salt, fanout, unit, count)

	// Compute prefix-sum write positions count[i] = i*unit so we can
	// scatter into sc.buffer / sc.offsetBuffer in one pass.
	for i, c := uint16(0), uint16(0); i < fanout; i++ {
		count[i] = c
		c += unit
	}
	for i := uint16(0); i < m; i++ {
		j := remap16(remix(bucket[i]+salt), m) / unit
		sc.buffer[count[j]] = bucket[i]
		sc.offsetBuffer[count[j]] = offsets[i]
		count[j]++
	}
	copy(bucket, sc.buffer[:m])
	copy(offsets, sc.offsetBuffer[:m])
	salt -= sc.startSeed[level]
	log2g := sc.golombParams.param(m)
	result.gr.appendFixed(salt, log2g)
	unary = append(unary, salt>>log2g)

	// Recurse over each `unit`-sized partition; the last partition
	// (size m - (fanout-1)*unit) is handled by a final tail block —
	// if it has 1 element, emit its offset directly; else recurse.
	var err error
	var i uint16
	for i = 0; i < m-unit; i += unit {
		if unary, err = recsplitRecurse(level+1, bucket[i:i+unit], offsets[i:i+unit], unary, sc, result); err != nil {
			return nil, err
		}
	}
	if m-i > 1 {
		if unary, err = recsplitRecurse(level+1, bucket[i:], offsets[i:], unary, sc, result); err != nil {
			return nil, err
		}
	} else if m-i == 1 {
		binary.BigEndian.PutUint64(sc.numBuf[:], offsets[i])
		result.offsetData = append(result.offsetData, sc.numBuf[8-sc.bytesPerRec:]...)
	}
	return unary, nil
}
