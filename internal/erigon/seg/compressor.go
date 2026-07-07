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

// Compress drains all queued words from the .idt, builds the position
// Huffman tree from the incrementally-accumulated posMap, and writes the
// final compressed file atomically to outputPath (no-pattern fast path,
// `compress.go:359-368 + parallel_compress.go:684-754`; see emitKV for
// the header/body layout).
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
	return c.emitKV(func(enc func([]byte) error) error {
		return c.uncompressedFile.ForEach(func(v []byte, _ bool) error {
			return enc(v)
		})
	})
}

// WordSource pushes a .kv's word sequence (key, value, key, value, …) in
// final order. It MUST be repeatable and deterministic: CompressFromSource
// invokes it TWICE (count pass, then encode pass) and any divergence
// between the passes silently corrupts the output. Producer errors are
// surfaced out-of-band by the caller (matching snap's entries convention).
type WordSource func(yield func(word []byte) bool)

// CompressFromSource writes the .kv directly from a repeatable word source,
// skipping the .idt intermediate entirely: pass A accumulates the counts +
// length histogram (exactly what AddWord does), pass B encodes straight
// into the output. Byte-identical to AddWord+Compress for the same word
// sequence — the .kv is a pure function of it (per-word encoding with the
// bit writer flushed to byte alignment after every word; the Huffman table
// derives only from the histogram). Requires a fresh Compressor.
func (c *Compressor) CompressFromSource(src WordSource) error {
	if c.closed {
		return ErrAlreadyClosed
	}
	if c.wordsCount != 0 {
		return errors.New("seg: CompressFromSource requires a fresh Compressor (no AddWord calls)")
	}
	src(func(w []byte) bool {
		c.wordsCount++
		c.countWord(len(w))
		return true
	})
	return c.emitKV(func(enc func([]byte) error) error {
		var encErr error
		var encoded uint64
		src(func(w []byte) bool {
			if err := enc(w); err != nil {
				encErr = err
				return false
			}
			encoded++
			return true
		})
		if encErr == nil && encoded != c.wordsCount {
			return fmt.Errorf("seg: source yielded %d words on the encode pass, %d on the count pass — source is not repeatable", encoded, c.wordsCount)
		}
		return encErr
	})
}

// emitKV writes the complete .kv — Huffman dict from the accumulated
// posMap, headers, per-word body, fsync + atomic rename. body(enc) must
// call enc(word) for every word in final order.
func (c *Compressor) emitKV(body func(enc func([]byte) error) error) error {
	// posMap + emptyWordsCount were accumulated incrementally (AddWord or
	// the CompressFromSource count pass) — the .idt is never rescanned for
	// them. The histogram fully determines the Huffman tree, so the output
	// is byte-identical to the old Pass-A scan.
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
	enc := func(v []byte) error {
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
	}
	if err := body(enc); err != nil {
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
