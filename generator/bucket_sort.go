package generator

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const (
	numBuckets      = 256
	entrySize       = hashSize + hashSize // 32 + 32 = 64 bytes per entry
	bucketBufSize   = 256 * 1024          // 256KB bufio buffer per bucket file
)

// bucketWriter distributes trie entries into 256 flat binary files by key[0].
// Each entry is written as a fixed 64-byte record: key[32] || value[32].
// This replaces the Pebble temp DB for external sorting.
type bucketWriter struct {
	dir     string
	files   [numBuckets]*os.File
	buffers [numBuckets]*bufio.Writer
	counts  [numBuckets]int64
	total   int64
}

func newBucketWriter(dir string) (*bucketWriter, error) {
	bw := &bucketWriter{dir: dir}
	for i := range numBuckets {
		path := filepath.Join(dir, fmt.Sprintf("bucket_%02x.bin", i))
		f, err := os.Create(path)
		if err != nil {
			bw.close()
			return nil, fmt.Errorf("create bucket file %02x: %w", i, err)
		}
		bw.files[i] = f
		bw.buffers[i] = bufio.NewWriterSize(f, bucketBufSize)
	}
	return bw, nil
}

func (bw *bucketWriter) writeEntries(entries []trieEntry) error {
	for i := range entries {
		bucket := entries[i].Key[0]
		if _, err := bw.buffers[bucket].Write(entries[i].Key[:]); err != nil {
			return err
		}
		if _, err := bw.buffers[bucket].Write(entries[i].Value[:]); err != nil {
			return err
		}
		bw.counts[bucket]++
		bw.total++
	}
	return nil
}

func (bw *bucketWriter) flush() error {
	for i := range numBuckets {
		if bw.buffers[i] != nil {
			if err := bw.buffers[i].Flush(); err != nil {
				return fmt.Errorf("flush bucket %02x: %w", i, err)
			}
		}
	}
	return nil
}

func (bw *bucketWriter) close() {
	for i := range numBuckets {
		if bw.files[i] != nil {
			bw.files[i].Close()
		}
	}
}

// loadBucket reads a bucket file into a pre-allocated slice and returns
// the populated sub-slice. The buf must have capacity >= bw.counts[bucket].
func loadBucket(dir string, bucket int, buf []trieEntry) ([]trieEntry, error) {
	path := filepath.Join(dir, fmt.Sprintf("bucket_%02x.bin", bucket))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return buf[:0], nil
		}
		return nil, fmt.Errorf("read bucket %02x: %w", bucket, err)
	}
	n := len(data) / entrySize
	if len(data)%entrySize != 0 {
		return nil, fmt.Errorf("bucket %02x: file size %d not a multiple of entry size %d", bucket, len(data), entrySize)
	}
	entries := buf[:n]
	for i := range n {
		off := i * entrySize
		copy(entries[i].Key[:], data[off:off+hashSize])
		copy(entries[i].Value[:], data[off+hashSize:off+entrySize])
	}
	return entries, nil
}

// sortBucket sorts entries by key using a uint64 fast-path comparator.
// SHA256 keys are uniformly distributed, so the first 8 bytes differ in
// >99.99% of pairs, avoiding the full bytes.Compare in the hot path.
func sortBucket(entries []trieEntry) {
	slices.SortFunc(entries, func(a, b trieEntry) int {
		aHi := binary.BigEndian.Uint64(a.Key[:8])
		bHi := binary.BigEndian.Uint64(b.Key[:8])
		if aHi < bHi {
			return -1
		}
		if aHi > bHi {
			return 1
		}
		return bytes.Compare(a.Key[:], b.Key[:])
	})
}

// bucketChainIterator implements ethdb.Iterator by concatenating sorted
// bucket files. It loads one bucket at a time, sorts it in memory, and
// iterates through the sorted entries. When a bucket is exhausted, it
// loads the next one. Double-buffering overlaps read+sort of the next
// bucket with iteration of the current one.
//
// This gives the streaming builder a single globally-sorted stream of
// entries, exactly like a Pebble iterator over a compacted DB.
type bucketChainIterator struct {
	dir    string
	counts [numBuckets]int64

	// Current bucket state
	curBucket int
	sorted    []trieEntry
	pos       int

	// Double-buffer: pre-loaded next bucket
	nextBucket int
	nextSorted []trieEntry
	nextErr    error
	nextReady  chan struct{}
	preloadWg  sync.WaitGroup

	// Shared buffer pool (two buffers, swapped)
	buf0, buf1 []trieEntry
	usingBuf0  bool

	// Current entry key/value for the Iterator interface
	curKey   []byte
	curValue []byte

	released bool
}

func newBucketChainIterator(dir string, counts [numBuckets]int64) *bucketChainIterator {
	// Find max bucket size for buffer allocation
	var maxCount int64
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	if maxCount == 0 {
		maxCount = 1
	}

	it := &bucketChainIterator{
		dir:       dir,
		counts:    counts,
		curBucket: -1, // will advance on first Next()
		buf0:      make([]trieEntry, maxCount),
		buf1:      make([]trieEntry, maxCount),
		usingBuf0: true,
	}

	// Pre-load the first non-empty bucket
	first := it.findNextNonEmpty(0)
	if first < numBuckets {
		it.nextBucket = first
		it.nextReady = make(chan struct{})
		it.preloadWg.Add(1)
		go func() {
			defer it.preloadWg.Done()
			it.nextSorted, it.nextErr = loadBucket(dir, first, it.buf1)
			if it.nextErr == nil {
				sortBucket(it.nextSorted)
			}
			close(it.nextReady)
		}()
	} else {
		it.curBucket = numBuckets // empty — no entries at all
	}

	return it
}

func (it *bucketChainIterator) findNextNonEmpty(from int) int {
	for i := from; i < numBuckets; i++ {
		if it.counts[i] > 0 {
			return i
		}
	}
	return numBuckets
}

func (it *bucketChainIterator) advanceBucket() bool {
	if it.nextReady == nil {
		return false
	}
	<-it.nextReady
	if it.nextErr != nil {
		return false
	}

	// Swap buffers: the current sorted data goes to the "old" buffer
	// which becomes available for the next preload.
	it.sorted = it.nextSorted
	it.pos = 0
	it.curBucket = it.nextBucket
	it.usingBuf0 = !it.usingBuf0

	// Start preloading the next non-empty bucket
	next := it.findNextNonEmpty(it.curBucket + 1)
	if next < numBuckets {
		it.nextBucket = next
		it.nextReady = make(chan struct{})
		// Pick the buffer NOT currently in use
		var buf []trieEntry
		if it.usingBuf0 {
			buf = it.buf1
		} else {
			buf = it.buf0
		}
		it.preloadWg.Add(1)
		go func() {
			defer it.preloadWg.Done()
			it.nextSorted, it.nextErr = loadBucket(it.dir, next, buf)
			if it.nextErr == nil {
				sortBucket(it.nextSorted)
			}
			close(it.nextReady)
		}()
	} else {
		it.nextReady = nil
	}

	return len(it.sorted) > 0
}

// --- ethdb.Iterator interface ---

func (it *bucketChainIterator) Next() bool {
	if it.released {
		return false
	}
	// Advance within current bucket
	if it.sorted != nil && it.pos < len(it.sorted) {
		it.curKey = it.sorted[it.pos].Key[:]
		it.curValue = it.sorted[it.pos].Value[:]
		it.pos++
		return true
	}
	// Try next bucket
	if !it.advanceBucket() {
		return false
	}
	if len(it.sorted) == 0 {
		return false
	}
	it.curKey = it.sorted[it.pos].Key[:]
	it.curValue = it.sorted[it.pos].Value[:]
	it.pos++
	return true
}

func (it *bucketChainIterator) Key() []byte   { return it.curKey }
func (it *bucketChainIterator) Value() []byte { return it.curValue }
func (it *bucketChainIterator) Error() error  { return it.nextErr }

func (it *bucketChainIterator) Release() {
	it.released = true
	it.preloadWg.Wait()
	it.sorted = nil
	it.nextSorted = nil
	it.buf0 = nil
	it.buf1 = nil
}

// Unused but required for the interface — forward-only iteration is sufficient.
func (it *bucketChainIterator) Seek(_ []byte) bool { return false }

// io.Writer noop for compatibility
func (it *bucketChainIterator) Write(p []byte) (n int, err error) { return 0, io.EOF }
