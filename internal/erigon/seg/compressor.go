package seg

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the user-facing tuning surface for NewCompressor. Mirrors
// `db/seg/compress.go:45-83 Cfg`. Zero-value fields are auto-filled with
// `DefaultConfig()` values when NewCompressor runs.
type Config struct {
	// MinPatternScore — patterns below this score are filtered out
	// during dict build. Default 1024 (matches Erigon).
	MinPatternScore uint64
	// MinPatternLen, MaxPatternLen — substring length window for the
	// pattern extractor. Default 5, 128.
	MinPatternLen, MaxPatternLen int
	// MaxDictPatterns — upper bound on dict size (post-reduction).
	// Default 64 * 1024.
	MaxDictPatterns int
	// Compression — per-word compression mask. State-actor v1 only
	// supports CompressNone (no pattern-cover encoding). Setting
	// CompressKeys / CompressVals returns ErrPatternCoverUnsupported
	// at NewCompressor time.
	Compression FileCompression
}

// DefaultConfig returns the same tuning as Erigon's `DefaultCfg`
// (`compress.go:90-99`), with Compression set to CompressNone.
func DefaultConfig() Config {
	return Config{
		MinPatternScore: defaultMinPatternScore,
		MinPatternLen:   defaultMinPatternLen,
		MaxPatternLen:   defaultMaxPatternLen,
		MaxDictPatterns: defaultMaxDictPatterns,
		Compression:     CompressNone,
	}
}

// ErrPatternCoverUnsupported signals an attempt to use pattern-cover
// compression (CompressKeys / CompressVals) which is deferred to v2
// (requires Patricia tree + MatchFinder3 + DP-cover algorithm).
var ErrPatternCoverUnsupported = errors.New(
	"seg: pattern-cover compression (CompressKeys/CompressVals) is not " +
		"implemented in v1; use CompressNone")

// ErrAlreadyClosed is returned by AddWord/Compress after Close.
var ErrAlreadyClosed = errors.New("seg: compressor closed")

// Compressor writes a .kv-style Erigon snapshot body file. Use:
//
//	c, err := NewCompressor(outPath, tmpDir, DefaultConfig())
//	// alternating key/value:
//	c.AddWord(k1); c.AddWord(v1); c.AddWord(k2); c.AddWord(v2)
//	c.Compress()  // performs Huffman + dict + final-file emission
//	c.Close()     // closes + removes the .idt temp file
//
// Not safe for concurrent use. Compress() must be called exactly once
// before Close().
type Compressor struct {
	cfg              Config
	uncompressedFile *rawWordsFile
	outputFile       string
	tmpDir           string
	wordsCount       uint64
	emptyWordsCount  uint64
	// posMap is the word-length histogram (posMap[len+1] per word, posMap[0] =
	// total words), maintained incrementally in AddWord so Compress needs no
	// extra scan of the .idt to rebuild it. It fully determines the position
	// Huffman tree, so the incremental value is byte-identical to a rescan.
	posMap map[uint64]uint64
	closed bool
}

// NewCompressor creates a Compressor writing to outputPath. tmpDir is
// used for the .idt scratch file (deleted on Close()).
//
// The output file is NOT created at NewCompressor time — Compress()
// writes to a `<outputPath>.tmp.<rand>` file and atomically renames on
// success.
func NewCompressor(outputPath, tmpDir string, cfg Config) (*Compressor, error) {
	// Backfill defaults for zero-valued fields. Matches Erigon's
	// behavior when a user passes Cfg{} explicitly.
	if cfg.MinPatternScore == 0 {
		cfg.MinPatternScore = defaultMinPatternScore
	}
	if cfg.MinPatternLen == 0 {
		cfg.MinPatternLen = defaultMinPatternLen
	}
	if cfg.MaxPatternLen == 0 {
		cfg.MaxPatternLen = defaultMaxPatternLen
	}
	if cfg.MaxDictPatterns == 0 {
		cfg.MaxDictPatterns = defaultMaxDictPatterns
	}
	// Reject pattern-cover modes — v1 only supports the no-pattern
	// fast path. CompressNone means "send all words through
	// AddUncompressedWord" semantically; AddWord still appends to the
	// .idt with the "compressed" flag bit, but Compress() detects no
	// patterns will be extracted and takes compressNoWordPatterns.
	if cfg.Compression != CompressNone {
		return nil, fmt.Errorf("%w (got 0x%02x)", ErrPatternCoverUnsupported, cfg.Compression)
	}

	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("seg: mkdir tmpDir: %w", err)
	}

	_, fileName := filepath.Split(outputPath)
	idtPath := filepath.Join(tmpDir, fileName) + ".idt"
	rwf, err := newRawWordsFile(idtPath)
	if err != nil {
		return nil, fmt.Errorf("seg: create raw words file: %w", err)
	}
	return &Compressor{
		cfg:              cfg,
		uncompressedFile: rwf,
		outputFile:       outputPath,
		tmpDir:           tmpDir,
		posMap:           make(map[uint64]uint64),
	}, nil
}

// countWord folds one word's length into the incremental posMap +
// emptyWordsCount that Compress would otherwise rebuild by rescanning the .idt.
func (c *Compressor) countWord(l int) {
	c.posMap[uint64(l)+1]++
	c.posMap[0]++
	if l == 0 {
		c.emptyWordsCount++
	}
}

// AddWord appends a word to the compressor's intermediate file. Words
// added via AddWord are flagged "compressed" — meaning the Huffman
// encoder MAY substitute pattern codes for substrings. In v1's
// no-pattern fast path, no patterns are ever extracted, so the encoded
// output is identical to AddUncompressedWord (only length + raw bytes).
//
// Erigon's `db/state/simple_accessor_builder.go:194-216` alternates
// AddWord(key) → AddWord(value) → AddWord(key) → ... for the .kv
// builder. This package preserves that convention.
func (c *Compressor) AddWord(word []byte) error {
	if c.closed {
		return ErrAlreadyClosed
	}
	c.wordsCount++
	c.countWord(len(word))
	return c.uncompressedFile.Append(word)
}

// AddUncompressedWord appends a word flagged "uncompressed" — even with
// pattern-cover enabled, the encoder will NOT substitute pattern codes
// for substrings of this word. In v1 (no patterns), there is no
// observable difference between AddWord and AddUncompressedWord on the
// wire.
func (c *Compressor) AddUncompressedWord(word []byte) error {
	if c.closed {
		return ErrAlreadyClosed
	}
	c.wordsCount++
	c.countWord(len(word))
	return c.uncompressedFile.AppendUncompressed(word)
}

// Compress drains all queued words, builds the position Huffman tree,
// and writes the final compressed file atomically to outputPath.
//
// Algorithm (no-pattern fast path, `compress.go:359-368 + parallel_compress.go:684-754`):
//  1. Flush .idt; scan it once to build posMap[length+1] = uses
//     (per-word) and a single posMap[0] = totalWords (terminator).
//  2. Build canonical Huffman tree over posMap (see huffman.go).
//  3. Write V1 header (version, featureFlagBitmask).
//  4. Write data header: 8B wordsCount, 8B emptyWordsCount, 8B
//     patternsSize=0, [no patterns], 8B posSize, posSize varint entries.
//  5. Per-word: encode pos2code[len+1] bits, optionally pos2code[0]
//     bits, flush, write raw bytes.
//  6. fsync + atomic rename.
//
// Compress() may be called only once. After it returns, the only legal
// call is Close().
func (c *Compressor) Compress() error {
	if c.closed {
		return ErrAlreadyClosed
	}
	// Flush .idt to disk so the forthcoming ForEach reads complete data.
	if err := c.uncompressedFile.Flush(); err != nil {
		return fmt.Errorf("seg: flush .idt: %w", err)
	}

	// posMap + emptyWordsCount were accumulated incrementally in AddWord, so
	// there is no need to rescan the .idt to rebuild them — one fewer full pass
	// over the 28-44 GiB intermediate per domain. The histogram fully determines
	// the Huffman tree, so the output is byte-identical to the old Pass-A scan.
	positionList, pos2code := buildPositionHuffman(c.posMap)

	// Open a temp file in the same directory for atomic rename.
	tmpFile, err := os.CreateTemp(filepath.Dir(c.outputFile), filepath.Base(c.outputFile)+".tmp.*")
	if err != nil {
		return fmt.Errorf("seg: create temp output: %w", err)
	}
	// os.CreateTemp hardcodes mode 0o600 regardless of umask. The
	// downstream Erigon daemon runs as a different uid (see commit
	// bd85125 / 05c3ebd context) and silently fails to open files it
	// can't read — chmod to 0o644 so the renamed final .kv is
	// world-readable.
	if err := tmpFile.Chmod(0o644); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("seg: chmod temp output: %w", err)
	}
	tmpFileName := tmpFile.Name()
	defer func() {
		// On any error after this point, remove the temp file.
		if tmpFile != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFileName)
		}
	}()

	cw := bufio.NewWriterSize(tmpFile, 256*1024)

	// V1 header: byte[0]=1, byte[1]=featureFlagBitmask (zero in v1).
	if _, err := cw.Write([]byte{fileCompressionFormatV1, 0}); err != nil {
		return fmt.Errorf("seg: write V1 header: %w", err)
	}

	var numBuf [8]byte
	// 8B BE wordsCount.
	binary.BigEndian.PutUint64(numBuf[:], c.wordsCount)
	if _, err := cw.Write(numBuf[:]); err != nil {
		return fmt.Errorf("seg: write wordsCount: %w", err)
	}
	// 8B BE emptyWordsCount.
	binary.BigEndian.PutUint64(numBuf[:], c.emptyWordsCount)
	if _, err := cw.Write(numBuf[:]); err != nil {
		return fmt.Errorf("seg: write emptyWordsCount: %w", err)
	}
	// 8B BE patternsSize = 0 (fast path).
	binary.BigEndian.PutUint64(numBuf[:], 0)
	if _, err := cw.Write(numBuf[:]); err != nil {
		return fmt.Errorf("seg: write patternsSize: %w", err)
	}

	// Compute posSize and write position dict.
	var posSize uint64
	var varBuf [binary.MaxVarintLen64]byte
	for _, p := range positionList {
		ns := binary.PutUvarint(varBuf[:], uint64(p.depth))
		n := binary.PutUvarint(varBuf[:], p.pos)
		posSize += uint64(ns + n)
	}
	binary.BigEndian.PutUint64(numBuf[:], posSize)
	if _, err := cw.Write(numBuf[:]); err != nil {
		return fmt.Errorf("seg: write posSize: %w", err)
	}
	for _, p := range positionList {
		ns := binary.PutUvarint(varBuf[:], uint64(p.depth))
		if _, err := cw.Write(varBuf[:ns]); err != nil {
			return fmt.Errorf("seg: write pos depth: %w", err)
		}
		n := binary.PutUvarint(varBuf[:], p.pos)
		if _, err := cw.Write(varBuf[:n]); err != nil {
			return fmt.Errorf("seg: write pos value: %w", err)
		}
	}

	// Pass B: encode each word's Huffman length code + raw bytes.
	hc := &bitWriter{w: cw}
	if err := c.uncompressedFile.ForEach(func(v []byte, _ bool) error {
		l := uint64(len(v))
		if pc := pos2code[l+1]; pc != nil {
			if e := hc.encode(pc.code, pc.codeBits); e != nil {
				return e
			}
		}
		if l == 0 {
			// Empty word: flush, no terminator, no raw bytes.
			return hc.flush()
		}
		// Non-empty: write terminator code, flush, then raw bytes.
		if pc := pos2code[0]; pc != nil {
			if e := hc.encode(pc.code, pc.codeBits); e != nil {
				return e
			}
		}
		if e := hc.flush(); e != nil {
			return e
		}
		_, e := cw.Write(v)
		return e
	}); err != nil {
		return fmt.Errorf("seg: encode body: %w", err)
	}

	if err := cw.Flush(); err != nil {
		return fmt.Errorf("seg: flush output: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("seg: fsync: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("seg: close temp: %w", err)
	}
	tmpFile = nil // prevent deferred cleanup
	if err := os.Rename(tmpFileName, c.outputFile); err != nil {
		_ = os.Remove(tmpFileName)
		return fmt.Errorf("seg: rename: %w", err)
	}
	return nil
}

// Close releases the .idt temp file (deleting it). Safe to call
// multiple times; subsequent calls are no-ops.
func (c *Compressor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.uncompressedFile != nil {
		c.uncompressedFile.CloseAndRemove()
		c.uncompressedFile = nil
	}
	return nil
}
