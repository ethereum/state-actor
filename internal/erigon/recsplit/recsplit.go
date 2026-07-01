package recsplit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
)

// ErrCollision is returned by Build() when two distinct keys produce the
// same (bucketIdx, fingerprintLo) tuple. The caller must bump the salt
// via *args.Salt++, Reset, re-add the keys, and retry. Matches Erigon's
// recsplit.ErrCollision at recsplit.go:51.
var ErrCollision = errors.New("duplicate key")

// Features mirror db/recsplit/index.go Features bitmap.
type Features byte

const (
	noFeatures         Features = 0
	featureEnums       Features = 0b1
	featureLessFalsePo Features = 0b10
)

// dataStructureVersion is the value we write at byte 0 of the .kvi. The
// spike currently mirrors the Erigon fixture which leaves
// args.Version=0 (the Default zero value). v0 has no fuse filter
// (legacy); v1 uses a monolithic FuseFilter for the existence filter.
// Since the spike has LessFalsePositives=false, the version byte does
// not affect the body layout — we just match the fixture's value.
//
// Once production wiring is added we'll thread the version through
// Args; for now the byte-equality target is v0 to keep the spike
// fixture lean.
const dataStructureVersion uint8 = 0

// DefaultStartSeed is Erigon's hardcoded 20-entry seed table. Used when
// caller doesn't supply one. Pinned to recsplit.go:247-249.
var DefaultStartSeed = []uint64{
	0x106393c187cae2a, 0x6453cec3f7376937, 0x643e521ddbd2be98, 0x3740c6412f6572cb, 0x717d47562f1ce470,
	0x4cd6eb4c63befb7c, 0x9bfd8c5e18c8da73, 0x082f20e10092a9a3, 0x2ada2ce68d21defc, 0xe33cb4f3e7c6466b,
	0x3980be458c509c59, 0xc466fd9584828e8c, 0x45f0aabe1a61ede6, 0xf6e7b8b33ad9b98d, 0x4ef95e25f4b4983d,
	0x81175195173b92d3, 0x4e50927d8dd15978, 0x1ea2099d1fafae7f, 0x425c8a06fbaaa815, 0xcd4216006c74052a,
}

// Args configures Writer. Mirrors db/recsplit/recsplit.go RecSplitArgs
// (recsplit.go:203-222), restricted to the spike subset:
//   - no enums (offsetEf encoding)
//   - no less-false-positives (existence filter)
//   - no parallel workers (always sequential)
//
// CRITICAL: Salt is a POINTER. Build() may discover a collision and the
// CALLER is expected to bump *Salt and retry. (Erigon's
// simple_accessor_builder.go:221 calls rs.ResetNextSalt() which mutates
// rs.salt in place; the Args.Salt pointer is captured at New().)
type Args struct {
	KeyCount   int
	BucketSize int
	Salt       *uint32
	LeafSize   uint16
	TmpDir     string
	IndexFile  string
	BaseDataID uint64
	Enums      bool // MUST be false in spike scope
}

// Writer is a single-use perfect-hash function builder. Construct with
// New, call AddKey N times, then Build.
type Writer struct {
	args     Args
	fileName string
	filePath string

	// Hash function state.
	startSeed []uint64

	// Bucket accumulation.
	collector       *bucketCollector
	bucketCount     uint64
	maxOffset       uint64
	keysAdded       uint64
	currentBucket   []uint64 // fingerprints in the bucket currently being processed
	currentBucketOs []uint64 // parallel offsets for currentBucket

	// Global Golomb-Rice bit-stream (one continuous stream across all buckets).
	gr GolombRice

	// Two parallel cumulative-accumulators encoded with DoubleEliasFano:
	//   bucketSizeAcc[i+1] = total keys in buckets [0..i]
	//   bucketPosAcc[i+1]  = total bits in `gr` after processing bucket i
	bucketSizeAcc []uint64
	bucketPosAcc  []uint64
	ef            DoubleEliasFano

	// Per-bucket scratch + golomb param cache.
	scratch *recsplitScratch

	// Index file output state.
	indexF *os.File
	indexW *bufio.Writer

	built     bool
	collision bool
}

// New constructs a Writer. Allocates `bucketCount = ceil(KeyCount/BucketSize)`
// counters. Mirrors recsplit.go:241-337.
func New(a Args) (*Writer, error) {
	if a.BaseDataID >= math.MaxUint64/2 {
		return nil, fmt.Errorf("recsplit: baseDataID %d too large", a.BaseDataID)
	}
	if a.LeafSize == 0 {
		a.LeafSize = 8
	}
	if a.LeafSize > MaxLeafSize {
		return nil, fmt.Errorf("recsplit: leafSize %d exceeds MaxLeafSize %d", a.LeafSize, MaxLeafSize)
	}
	if a.BucketSize == 0 {
		a.BucketSize = 100
	}
	if a.Enums {
		return nil, errors.New("recsplit: Enums=true is out of spike scope (offsetEf not implemented)")
	}

	bucketCount := (a.KeyCount + a.BucketSize - 1) / a.BucketSize
	if uint64(bucketCount) > math.MaxUint32 {
		return nil, fmt.Errorf("recsplit: bucketCount %d exceeds uint32", bucketCount)
	}

	// Salt resolution: caller-supplied pointer is mutable across
	// collision retries. nil means generate a random salt.
	saltPtr := a.Salt
	if saltPtr == nil {
		var buf [4]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("recsplit: rand: %w", err)
		}
		s := binary.BigEndian.Uint32(buf[:])
		saltPtr = &s
		a.Salt = saltPtr
	}

	_, fname := filepath.Split(a.IndexFile)
	collector, err := newBucketCollector(a.TmpDir)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		args:        a,
		fileName:    fname,
		filePath:    a.IndexFile,
		startSeed:   DefaultStartSeed,
		collector:   collector,
		bucketCount: uint64(bucketCount),
	}

	primary, secondary := computeAggrBounds(a.LeafSize)
	w.scratch = &recsplitScratch{
		count:              make([]uint16, 8*secondary), // 8-way salt parallelism in findSplit
		startSeed:          w.startSeed,
		leafSize:           a.LeafSize,
		primaryAggrBound:   primary,
		secondaryAggrBound: secondary,
		golombParams: &golombParamCache{
			leafSize:           a.LeafSize,
			primaryAggrBound:   primary,
			secondaryAggrBound: secondary,
		},
	}

	w.currentBucket = make([]uint64, 0, a.BucketSize)
	w.currentBucketOs = make([]uint64, 0, a.BucketSize)

	// First entry of each accumulator is always 0.
	w.bucketSizeAcc = make([]uint64, 1, bucketCount+1)
	w.bucketPosAcc = make([]uint64, 1, bucketCount+1)

	return w, nil
}

// Salt returns the salt currently in use (reads through the pointer).
func (w *Writer) Salt() uint32 { return *w.args.Salt }

// KeyCount returns the number of keys added so far.
func (w *Writer) KeyCount() uint64 { return w.keysAdded }

// BucketCount returns the total bucket count.
func (w *Writer) BucketCount() uint64 { return w.bucketCount }

// AddKey records one (key, offset) pair. Hashes key with the current
// salt to derive (bucketIdx, fingerprintLo); buffers the triple for
// Build to sort + emit.
//
// Mirrors recsplit.go:517-580 (the non-enum, non-lfp path).
func (w *Writer) AddKey(key []byte, offset uint64) error {
	if w.built {
		return errors.New("recsplit: AddKey after Build")
	}
	hi, lo := keyHash(key, *w.args.Salt)
	bucketIdx := uint32(remap(hi, w.bucketCount))
	if offset > w.maxOffset {
		w.maxOffset = offset
	}
	if err := w.collector.Add(bucketIdx, lo, offset); err != nil {
		return err
	}
	w.keysAdded++
	return nil
}

// Reset clears the buffered keys + accumulators in preparation for a
// salt-bump retry. The caller is responsible for `*w.args.Salt++` before
// calling this. Mirrors recsplit.go:437-461 (ResetNextSalt).
func (w *Writer) Reset() error {
	w.built = false
	w.collision = false
	w.keysAdded = 0
	w.maxOffset = 0
	if err := w.collector.Reset(); err != nil {
		return err
	}
	w.currentBucket = w.currentBucket[:0]
	w.currentBucketOs = w.currentBucketOs[:0]
	w.bucketSizeAcc = w.bucketSizeAcc[:1]
	w.bucketPosAcc = w.bucketPosAcc[:1]
	w.gr = GolombRice{}
	w.ef = DoubleEliasFano{}
	return nil
}

// Build runs the perfect-hash construction and writes the .kvi file
// atomically (via a tmp file + rename).
//
// Returns ErrCollision if any two keys collided in (bucketIdx,
// fingerprintLo). The caller should bump *w.args.Salt, call Reset, re-add
// keys, and call Build again.
//
// Mirrors recsplit.go:917-1095 (the sequential, no-enum path).
func (w *Writer) Build(ctx context.Context) error {
	if w.built {
		return errors.New("recsplit: already built")
	}
	if uint64(w.args.KeyCount) != w.keysAdded {
		return fmt.Errorf("recsplit: expected %d keys, got %d", w.args.KeyCount, w.keysAdded)
	}

	// Open index temp file (atomic-rename pattern).
	var err error
	w.indexF, err = os.CreateTemp(filepath.Dir(w.filePath), filepath.Base(w.filePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("recsplit: create temp: %w", err)
	}
	// os.CreateTemp hardcodes 0o600 — the daemon (separate uid)
	// can't read it. Chmod to 0o644 before the rename so the final
	// .kvi is readable.
	if err := w.indexF.Chmod(0o644); err != nil {
		_ = w.indexF.Close()
		_ = os.Remove(w.indexF.Name())
		w.indexF = nil
		return fmt.Errorf("recsplit: chmod temp: %w", err)
	}
	defer func() {
		if w.indexF != nil {
			_ = w.indexF.Close()
			_ = os.Remove(w.indexF.Name())
			w.indexF = nil
		}
	}()
	w.indexW = bufio.NewWriter(w.indexF)

	// Header (17 bytes):
	//   1B  dataStructureVersion
	//   7B  baseDataID (BE; numBuf[0] is overwritten by version byte)
	//   8B  keyCount   (BE)
	//   1B  bytesPerRec
	var numBuf [8]byte
	binary.BigEndian.PutUint64(numBuf[:], w.args.BaseDataID)
	numBuf[0] = dataStructureVersion
	if _, err := w.indexW.Write(numBuf[:]); err != nil {
		return err
	}
	binary.BigEndian.PutUint64(numBuf[:], w.keysAdded)
	if _, err := w.indexW.Write(numBuf[:]); err != nil {
		return err
	}
	// Non-enum: bytesPerRec is BitLenToByteLen(bits.Len64(maxOffset)).
	bytesPerRec := bitLenToByteLen(bits.Len64(w.maxOffset))
	w.scratch.bytesPerRec = bytesPerRec
	if err := w.indexW.WriteByte(byte(bytesPerRec)); err != nil {
		return err
	}

	// Body: walk buckets in (bucketIdx, fingerprintLo) order; for each
	// bucket, recsplit it, append fixed GolombRice + collect unary into
	// w.gr; write the bucket's serialized offsets into indexW.
	if err := w.collector.Finalize(); err != nil {
		return err
	}
	prevBucketIdx := ^uint32(0) // sentinel for "first bucket"
	if err := w.collector.ForEach(func(e bucketEntry) error {
		if e.bucketIdx != prevBucketIdx {
			if prevBucketIdx != ^uint32(0) {
				if err := w.flushCurrentBucket(uint64(prevBucketIdx)); err != nil {
					return err
				}
			}
			prevBucketIdx = e.bucketIdx
		}
		w.currentBucket = append(w.currentBucket, e.fingerprintLo)
		w.currentBucketOs = append(w.currentBucketOs, e.offset)

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		return nil
	}); err != nil {
		return err
	}
	// Flush the final bucket.
	if len(w.currentBucket) > 0 {
		if err := w.flushCurrentBucket(uint64(prevBucketIdx)); err != nil {
			return err
		}
	}

	// Sentinel: appendFixed(1, 1). Avoids checking for parts of size 1
	// on the reader side. Mirrors recsplit.go:1009.
	w.gr.appendFixed(1, 1)

	// Build the DoubleEliasFano over bucketSizeAcc + bucketPosAcc.
	// Both must have length bucketCount+1 (Erigon pads with the last
	// value if no key landed in some trailing buckets).
	for len(w.bucketSizeAcc) <= int(w.bucketCount) {
		w.bucketSizeAcc = append(w.bucketSizeAcc, w.bucketSizeAcc[len(w.bucketSizeAcc)-1])
	}
	for len(w.bucketPosAcc) <= int(w.bucketCount) {
		w.bucketPosAcc = append(w.bucketPosAcc, w.bucketPosAcc[len(w.bucketPosAcc)-1])
	}
	w.ef.Build(w.bucketSizeAcc, w.bucketPosAcc)
	w.built = true

	// Footer:
	binary.BigEndian.PutUint64(numBuf[:], w.bucketCount)
	if _, err := w.indexW.Write(numBuf[:8]); err != nil {
		return err
	}
	binary.BigEndian.PutUint16(numBuf[:], uint16(w.args.BucketSize))
	if _, err := w.indexW.Write(numBuf[:2]); err != nil {
		return err
	}
	binary.BigEndian.PutUint16(numBuf[:], w.args.LeafSize)
	if _, err := w.indexW.Write(numBuf[:2]); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(numBuf[:], *w.args.Salt)
	if _, err := w.indexW.Write(numBuf[:4]); err != nil {
		return err
	}
	if err := w.indexW.WriteByte(byte(len(w.startSeed))); err != nil {
		return err
	}
	for _, s := range w.startSeed {
		binary.BigEndian.PutUint64(numBuf[:], s)
		if _, err := w.indexW.Write(numBuf[:]); err != nil {
			return err
		}
	}

	// Features byte (always 0 in spike: no enums, no LFP).
	if err := w.indexW.WriteByte(byte(noFeatures)); err != nil {
		return err
	}

	// Golomb-rice param count (uint16, written as 2B BE in a 4B slot —
	// recsplit.go:1065-1068 writes 4 bytes but uint16 only fills lo 2).
	//
	// CRITICAL: Erigon writes 4 bytes via `rs.indexW.Write(rs.numBuf[:4])`
	// after `binary.BigEndian.PutUint16(rs.numBuf[:], ...)`. PutUint16
	// only writes bytes 0..1; bytes 2..3 retain whatever was there from
	// the LAST 8B write (the previous startSeed). That stale-byte
	// behavior is wire-observable. We mimic it by reusing numBuf without
	// clearing.
	binary.BigEndian.PutUint16(numBuf[:], uint16(len(w.scratch.golombParams.table)))
	if _, err := w.indexW.Write(numBuf[:4]); err != nil {
		return err
	}

	// GolombRice payload.
	if err := w.gr.Write(w.indexW); err != nil {
		return err
	}
	// DoubleEliasFano payload.
	if err := w.ef.Write(w.indexW); err != nil {
		return err
	}

	if err := w.indexW.Flush(); err != nil {
		return err
	}
	tmpName := w.indexF.Name()
	if err := w.indexF.Close(); err != nil {
		return err
	}
	w.indexF = nil
	if err := os.Rename(tmpName, w.filePath); err != nil {
		return err
	}
	return nil
}

// flushCurrentBucket processes w.currentBucket as bucket-index
// `bucketIdx`, appends its offset bytes to the index file, and updates
// the cumulative accumulators. Port of recsplit.go:595-651.
func (w *Writer) flushCurrentBucket(bucketIdx uint64) error {
	// Extend bucketSizeAcc to cover bucketIdx (padding gaps with the
	// previous cumulative value — empty buckets contribute zero keys).
	for len(w.bucketSizeAcc) <= int(bucketIdx)+1 {
		w.bucketSizeAcc = append(w.bucketSizeAcc, w.bucketSizeAcc[len(w.bucketSizeAcc)-1])
	}
	w.bucketSizeAcc[int(bucketIdx)+1] += uint64(len(w.currentBucket))

	res := &bucketResult{
		offsetData: make([]byte, 0, len(w.currentBucket)*w.scratch.bytesPerRec),
	}
	if len(w.currentBucket) > 1 {
		// Collision check inside the bucket.
		for i := 1; i < len(w.currentBucket); i++ {
			if w.currentBucket[i] == w.currentBucket[i-1] {
				w.collision = true
				return fmt.Errorf("%w: fingerprint %x in bucket %d",
					ErrCollision, w.currentBucket[i], bucketIdx)
			}
		}
		w.scratch.preAlloc(len(w.currentBucket))
		unary := make([]uint64, 0, len(w.currentBucket))
		var err error
		unary, err = recsplitRecurse(0, w.currentBucket, w.currentBucketOs, unary, w.scratch, res)
		if err != nil {
			return err
		}
		w.gr.Append(&res.gr)
		w.gr.appendUnaryAll(unary)
	} else {
		// Size 0 or 1: just emit the offset directly.
		var numBuf [8]byte
		for _, off := range w.currentBucketOs {
			binary.BigEndian.PutUint64(numBuf[:], off)
			res.offsetData = append(res.offsetData, numBuf[8-w.scratch.bytesPerRec:]...)
		}
	}

	if _, err := w.indexW.Write(res.offsetData); err != nil {
		return err
	}

	for len(w.bucketPosAcc) <= int(bucketIdx)+1 {
		w.bucketPosAcc = append(w.bucketPosAcc, w.bucketPosAcc[len(w.bucketPosAcc)-1])
	}
	w.bucketPosAcc[int(bucketIdx)+1] = uint64(w.gr.Bits())

	w.currentBucket = w.currentBucket[:0]
	w.currentBucketOs = w.currentBucketOs[:0]
	return nil
}

// Close releases any open files.
func (w *Writer) Close() error {
	if w.indexF != nil {
		_ = w.indexF.Close()
		_ = os.Remove(w.indexF.Name())
		w.indexF = nil
	}
	if w.collector != nil {
		_ = w.collector.Close()
	}
	return nil
}

// Collision returns true if Build saw a (bucketIdx, fingerprintLo)
// collision. Caller should bump salt and retry.
func (w *Writer) Collision() bool { return w.collision }

// bitLenToByteLen is Erigon's common.BitLenToByteLen — rounds bits up to
// the nearest byte. (bits+7)>>3, with a special case for 0.
func bitLenToByteLen(bitLen int) int {
	if bitLen == 0 {
		return 0
	}
	return (bitLen + 7) >> 3
}
