package seg

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRoundTripBasic writes a 10-word .kv file, then reads it back via
// Iterate, asserting (1) every key/value matches, (2) byte offsets are
// strictly increasing, (3) the wordsCount/emptyCount header values
// match what was written.
func TestRoundTripBasic(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.kv")
	c, err := NewCompressor(out, dir, DefaultConfig())
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	defer c.Close()

	pairs := [][2][]byte{
		{[]byte("key01"), []byte("value-one")},
		{[]byte("key02"), []byte("value-two-longer")},
		{[]byte("key03"), []byte("v3")},
		{[]byte("key04"), []byte("value-four")},
		{[]byte("k5"), []byte("v5")},
	}
	for _, kv := range pairs {
		if err := c.AddWord(kv[0]); err != nil {
			t.Fatalf("AddWord key: %v", err)
		}
		if err := c.AddWord(kv[1]); err != nil {
			t.Fatalf("AddWord val: %v", err)
		}
	}
	if err := c.Compress(); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Size() < 32 {
		t.Fatalf("output too small: %d bytes", info.Size())
	}

	d, err := NewDecompressor(out)
	if err != nil {
		t.Fatalf("NewDecompressor: %v", err)
	}
	defer d.Close()
	if got, want := d.Count(), uint64(len(pairs)*2); got != want {
		t.Errorf("Count: got %d, want %d", got, want)
	}
	if got, want := d.EmptyCount(), uint64(0); got != want {
		t.Errorf("EmptyCount: got %d, want %d", got, want)
	}

	var lastValOff uint64
	i := 0
	for entry, err := range d.Iterate(context.Background()) {
		if err != nil {
			t.Fatalf("Iterate yielded error at idx %d: %v", i, err)
		}
		if i >= len(pairs) {
			t.Fatalf("Iterate yielded more entries than expected; got idx=%d", i)
		}
		if !bytes.Equal(entry.Key, pairs[i][0]) {
			t.Errorf("key[%d]: got %q, want %q", i, entry.Key, pairs[i][0])
		}
		if !bytes.Equal(entry.Value, pairs[i][1]) {
			t.Errorf("val[%d]: got %q, want %q", i, entry.Value, pairs[i][1])
		}
		if entry.KeyOffset >= entry.ValueOffset {
			t.Errorf("entry[%d]: KeyOffset %d should be < ValueOffset %d",
				i, entry.KeyOffset, entry.ValueOffset)
		}
		if i > 0 && entry.KeyOffset <= lastValOff {
			t.Errorf("entry[%d]: KeyOffset %d should be > previous valOff %d",
				i, entry.KeyOffset, lastValOff)
		}
		lastValOff = entry.ValueOffset
		i++
	}
	if i != len(pairs) {
		t.Errorf("Iterate yielded %d entries, want %d", i, len(pairs))
	}
}

// TestEmptyWordsCount verifies that adding empty-byte-value words is
// counted in emptyCount and round-trips correctly.
func TestEmptyWordsCount(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "empty.kv")
	c, err := NewCompressor(out, dir, DefaultConfig())
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	defer c.Close()
	// Two pairs, each with an empty value.
	pairs := [][2][]byte{
		{[]byte("k1"), nil},
		{[]byte("k2"), []byte{}},
	}
	for _, kv := range pairs {
		if err := c.AddWord(kv[0]); err != nil {
			t.Fatalf("AddWord: %v", err)
		}
		if err := c.AddWord(kv[1]); err != nil {
			t.Fatalf("AddWord: %v", err)
		}
	}
	if err := c.Compress(); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	c.Close()
	d, err := NewDecompressor(out)
	if err != nil {
		t.Fatalf("NewDecompressor: %v", err)
	}
	defer d.Close()
	if got := d.EmptyCount(); got != 2 {
		t.Errorf("EmptyCount: got %d, want 2", got)
	}
}

// TestPatternCoverRejected verifies that requesting CompressKeys or
// CompressVals returns an explicit error (v1 only ships CompressNone).
func TestPatternCoverRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Compression = CompressKeys
	_, err := NewCompressor(filepath.Join(dir, "k.kv"), dir, cfg)
	if !errors.Is(err, ErrPatternCoverUnsupported) {
		t.Fatalf("got err %v, want ErrPatternCoverUnsupported", err)
	}
	cfg.Compression = CompressVals
	_, err = NewCompressor(filepath.Join(dir, "v.kv"), dir, cfg)
	if !errors.Is(err, ErrPatternCoverUnsupported) {
		t.Fatalf("got err %v, want ErrPatternCoverUnsupported", err)
	}
}

// TestAddUncompressedWordRoundTrips ensures the AddUncompressedWord
// path (which sets the .idt entry's "uncompressed" flag) round-trips
// identically to AddWord in v1 (no patterns → both flags are
// observationally equivalent).
func TestAddUncompressedWordRoundTrips(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ucw.kv")
	c, err := NewCompressor(out, dir, DefaultConfig())
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	defer c.Close()
	// Pair 0: AddWord for both.
	if err := c.AddWord([]byte("k1")); err != nil {
		t.Fatalf("AddWord: %v", err)
	}
	if err := c.AddWord([]byte("v1")); err != nil {
		t.Fatalf("AddWord: %v", err)
	}
	// Pair 1: AddUncompressedWord for the value.
	if err := c.AddWord([]byte("k2")); err != nil {
		t.Fatalf("AddWord: %v", err)
	}
	if err := c.AddUncompressedWord([]byte("v2-uncompressed-marker")); err != nil {
		t.Fatalf("AddUncompressedWord: %v", err)
	}
	if err := c.Compress(); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	c.Close()
	d, err := NewDecompressor(out)
	if err != nil {
		t.Fatalf("NewDecompressor: %v", err)
	}
	defer d.Close()
	if got := d.Count(); got != 4 {
		t.Fatalf("Count: got %d, want 4", got)
	}
	want := [][2]string{
		{"k1", "v1"},
		{"k2", "v2-uncompressed-marker"},
	}
	i := 0
	for entry, err := range d.Iterate(context.Background()) {
		if err != nil {
			t.Fatalf("iterate err: %v", err)
		}
		if got := string(entry.Key); got != want[i][0] {
			t.Errorf("[%d] key: got %q, want %q", i, got, want[i][0])
		}
		if got := string(entry.Value); got != want[i][1] {
			t.Errorf("[%d] val: got %q, want %q", i, got, want[i][1])
		}
		i++
	}
	if i != 2 {
		t.Fatalf("iterated %d, want 2", i)
	}
}

// TestNewDecompressorRejectsShortFile verifies the < compressedMinSize
// guard at the top of NewDecompressor.
func TestNewDecompressorRejectsShortFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "short.kv")
	// Write a 10-byte file (less than the 32-byte minimum).
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := NewDecompressor(p)
	if !errors.Is(err, ErrCorruptedFile) {
		t.Fatalf("want ErrCorruptedFile, got %v", err)
	}
}

// TestNewDecompressorRejectsBadVersion verifies the version-byte guard.
func TestNewDecompressorRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v99.kv")
	// 32-byte file with version byte 0x99.
	buf := make([]byte, 32)
	buf[0] = 0x99
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := NewDecompressor(p)
	if !errors.Is(err, ErrCorruptedFile) {
		t.Fatalf("want ErrCorruptedFile, got %v", err)
	}
}

// TestNewDecompressorRejectsNonzeroFlags verifies that unsupported
// feature-flag bits cause a decode error (v1 supports only flags == 0).
func TestNewDecompressorRejectsNonzeroFlags(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "flags.kv")
	buf := make([]byte, 64)
	buf[0] = 0x01 // V1
	buf[1] = 0x01 // PageLevelCompression bit
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := NewDecompressor(p)
	if !errors.Is(err, ErrCorruptedFile) {
		t.Fatalf("want ErrCorruptedFile, got %v", err)
	}
}

// TestAddWordAfterCloseFails verifies the closed-state guard.
func TestAddWordAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCompressor(filepath.Join(dir, "x.kv"), dir, DefaultConfig())
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.AddWord([]byte("k")); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("AddWord after close: want ErrAlreadyClosed, got %v", err)
	}
	if err := c.AddUncompressedWord([]byte("v")); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("AddUncompressedWord after close: want ErrAlreadyClosed, got %v", err)
	}
	if err := c.Compress(); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("Compress after close: want ErrAlreadyClosed, got %v", err)
	}
}

// TestFileCompressionHas verifies the Has helper on the bit-mask type.
func TestFileCompressionHas(t *testing.T) {
	both := CompressKeys | CompressVals
	if !both.Has(CompressKeys) {
		t.Error("Keys|Vals should Has(Keys)")
	}
	if !both.Has(CompressVals) {
		t.Error("Keys|Vals should Has(Vals)")
	}
	if CompressNone.Has(CompressKeys) {
		t.Error("None should not Has(Keys)")
	}
}

// TestCloseAfterCompress verifies multiple Close calls are safe.
func TestCloseAfterCompress(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCompressor(filepath.Join(dir, "x.kv"), dir, DefaultConfig())
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	if err := c.AddWord([]byte("k")); err != nil {
		t.Fatalf("AddWord: %v", err)
	}
	if err := c.AddWord([]byte("v")); err != nil {
		t.Fatalf("AddWord: %v", err)
	}
	if err := c.Compress(); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 2 (idempotent): %v", err)
	}
}

// fixture mirrors the schema produced by
// upstream Erigon's reference seg encoder.
type fixture struct {
	Label         string     `json:"label"`
	Words         [][]string `json:"words"` // list of (key_hex, value_hex) pairs
	ExpectedKvHex string     `json:"expected_kv_hex"`
}

// TestGoldenAgainstErigon is the load-bearing byte-equality test:
// for every fixture captured by running Erigon's seg.Compressor (see
// upstream Erigon's reference seg encoder), our pure-Go Compressor
// must produce byte-identical output. If the golden file is missing
// (oracle host not provisioned yet), the test is skipped with a
// regeneration hint instead of failing — matching the eliasfano /
// account pattern.
//
// The golden fixtures are committed under testdata/; they were captured
// from upstream Erigon v3.4.2's reference seg compressor.
func TestGoldenAgainstErigon(t *testing.T) {
	path := filepath.Join("testdata", "erigon_golden.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("seg/testdata/erigon_golden.json missing; golden is committed under testdata/")
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var fixtures []fixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures in golden file")
	}
	for _, f := range fixtures {
		f := f
		t.Run(f.Label, func(t *testing.T) {
			want, err := hex.DecodeString(f.ExpectedKvHex)
			if err != nil {
				t.Fatalf("decode expected hex: %v", err)
			}
			var words [][]byte
			for i, pair := range f.Words {
				if len(pair) != 2 {
					t.Fatalf("fixture %s: word %d has %d fields, want 2", f.Label, i, len(pair))
				}
				k, err := hex.DecodeString(pair[0])
				if err != nil {
					t.Fatalf("fixture %s: decode key %d hex: %v", f.Label, i, err)
				}
				v, err := hex.DecodeString(pair[1])
				if err != nil {
					t.Fatalf("fixture %s: decode val %d hex: %v", f.Label, i, err)
				}
				words = append(words, k, v)
			}
			// Both writer paths must produce the same upstream golden
			// bytes: AddWord+Compress (.idt) and the .idt-free
			// CompressFromSource two-pass path.
			dir := t.TempDir()
			outPath := filepath.Join(dir, f.Label+".kv")
			c, err := NewCompressor(outPath, dir, DefaultConfig())
			if err != nil {
				t.Fatalf("NewCompressor: %v", err)
			}
			defer c.Close()
			for i, w := range words {
				if err := c.AddWord(w); err != nil {
					t.Fatalf("AddWord %d: %v", i, err)
				}
			}
			if err := c.Compress(); err != nil {
				t.Fatalf("Compress: %v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			got, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatalf("read output: %v", err)
			}
			srcPath := filepath.Join(dir, f.Label+".src.kv")
			cs, err := NewCompressor(srcPath, dir, DefaultConfig())
			if err != nil {
				t.Fatalf("NewCompressor(source): %v", err)
			}
			defer cs.Close()
			if err := cs.CompressFromSource(func(yield func([]byte) bool) {
				for _, w := range words {
					if !yield(w) {
						return
					}
				}
			}); err != nil {
				t.Fatalf("CompressFromSource: %v", err)
			}
			if err := cs.Close(); err != nil {
				t.Fatalf("Close(source): %v", err)
			}
			if gotSrc, err := os.ReadFile(srcPath); err != nil {
				t.Fatalf("read source output: %v", err)
			} else if !bytes.Equal(gotSrc, want) {
				t.Fatalf("CompressFromSource bytes diverge from golden for %s (len got=%d want=%d)",
					f.Label, len(gotSrc), len(want))
			}
			if !bytes.Equal(got, want) {
				firstDiff := -1
				n := len(got)
				if len(want) < n {
					n = len(want)
				}
				for i := 0; i < n; i++ {
					if got[i] != want[i] {
						firstDiff = i
						break
					}
				}
				t.Fatalf("byte mismatch for %s: first differs at byte %d\n  got len=%d %x\n  want len=%d %x",
					f.Label, firstDiff, len(got), got, len(want), want)
			}
		})
	}
}
