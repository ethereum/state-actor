package btindex

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethereum/state-actor/internal/erigon/eliasfano"
)

// DefaultBtreeM mirrors Erigon's `DefaultBtreeM` at btree_index.go:52.
// 256 keys per leaf is the production value (`BT_M` env-var override
// in Erigon is not mirrored here; state-actor callers pass M explicitly).
const DefaultBtreeM uint16 = 256

// Args configures a Writer. Fields mirror `BtIndexWriterArgs` at
// btree_index.go:192-199, minus the salt (Verifier B correction:
// the BTree itself doesn't use a salt; salt belongs to the `.kvei`
// existence-filter pre-hash on the caller side).
type Args struct {
	// KeyCount is the EXACT number of keys the caller will AddKey.
	// Used to size the embedded EliasFano builder. Mismatch causes
	// Build to fail.
	KeyCount int
	// M is the BTree branching factor — every M-th key (plus the
	// first key) is cached as a Node in the sparse index. Default
	// 256 (DefaultBtreeM); callers should generally not override.
	M uint16
	// TmpDir is the directory for the offsets spill file. AddKey streams
	// every offset (8 bytes/key) to a temp file under TmpDir instead of
	// an in-memory slice, so peak RAM stays O(bufio buffer) regardless of
	// KeyCount (the offsets at 309M keys would otherwise be ~2.5 GiB).
	// Build replays the file once into the EliasFano builder. If TmpDir
	// is "", the spill file is created next to IndexFile.
	TmpDir string
	// IndexFile is the final destination path of the `.bt` file
	// (e.g., `…/v1.0-accounts.0-256.bt`). The tmp file is created
	// next to IndexFile (in the same directory).
	IndexFile string
}

// Sentinel errors. Erigon panics; we return errors per state-actor
// convention.
var (
	// ErrAddKeyAfterBuild is returned when AddKey is called after Build.
	// Mirrors `btree_index.go:227`.
	ErrAddKeyAfterBuild = errors.New("btindex: cannot add keys after Build")
	// ErrAlreadyBuilt is returned when Build is called more than once.
	// Mirrors `btree_index.go:261`.
	ErrAlreadyBuilt = errors.New("btindex: already built")
	// ErrTooManyKeys is returned when AddKey is called more times than
	// the KeyCount declared in Args.
	ErrTooManyKeys = errors.New("btindex: too many keys for declared count")
	// ErrCountMismatch is returned by Build when fewer keys than
	// KeyCount were added.
	ErrCountMismatch = errors.New("btindex: AddKey called fewer times than KeyCount")
	// ErrNonMonotonicOffset is returned when AddKey receives an offset
	// smaller than the previously-added offset. Erigon's flow always
	// streams from a sequential `.kv` reader so offsets are naturally
	// monotonic; we enforce the invariant explicitly because the
	// embedded EliasFano builder relies on it.
	ErrNonMonotonicOffset = errors.New("btindex: offsets must be monotonically non-decreasing")
	// ErrEmptyKey rejects zero-length keys; the first-byte b0 sentinel
	// requires key[0] which would panic on an empty slice.
	ErrEmptyKey = errors.New("btindex: key must be non-empty")
)

// Writer emits a `.bt` BTree accessor file matching Erigon's wire format.
// Single-use: New → AddKey×N → Build → Close.
type Writer struct {
	args Args

	// In-flight builder state.
	nodes     []node // sparse-index cache; one entry per M-th key + first key
	maxOffset uint64
	prevOff   uint64
	written   uint64 // count of AddKey calls so far

	// offsets spill (B1): every offset is streamed (8-byte LE) to a temp
	// file instead of an in-memory []uint64, then replayed once in Build.
	// Lazily created on the first AddKey, so KeyCount==0 never opens a
	// file (preserving the zero-byte-index path). offScratch is the
	// reusable 8-byte encode buffer.
	offsetsPath string
	offsetsFile *os.File
	offsetsW    *bufio.Writer
	offScratch  [8]byte

	// b0 mirrors `BuildBtreeIndexWithDecompressor`'s 256-element
	// fresh-top-byte sentinel (`btree_index.go:424`). Encapsulated
	// inside the writer so callers don't need to pass `keep`.
	b0 [256]bool

	built bool
}

// node mirrors `bps_tree.go:143-147` (the unexported `Node` struct in
// Erigon). Field names lowercase to match.
type node struct {
	key []byte
	di  uint64 // key ordinal
}

// New constructs a Writer. The output `.bt` file at args.IndexFile is
// created lazily inside Build (Erigon's pattern: `dir.CreateTemp` is
// called from Build, not the constructor — see `btree_index.go:264`).
//
// Returns an error on bad input (KeyCount<0, empty IndexFile).
// State-actor's signature differs from Erigon's `NewBtIndexWriter` (no
// logger; M is a uint16 in Args rather than the env-driven `DefaultBtreeM`
// global; salt is not threaded through this primitive).
func New(a Args) (*Writer, error) {
	if a.KeyCount < 0 {
		return nil, fmt.Errorf("btindex: KeyCount must be non-negative, got %d", a.KeyCount)
	}
	if a.IndexFile == "" {
		return nil, errors.New("btindex: IndexFile must be non-empty")
	}
	if a.M == 0 {
		a.M = DefaultBtreeM
	}
	w := &Writer{
		args:  a,
		nodes: make([]node, 0, (a.KeyCount/int(a.M))+1),
	}
	return w, nil
}

// appendOffset streams one offset to the spill file, lazily creating it
// on the first call. The file lands under args.TmpDir (or next to
// IndexFile when TmpDir is ""), matching where Build writes its own
// .bt temp file.
func (w *Writer) appendOffset(off uint64) error {
	if w.offsetsW == nil {
		dir := w.args.TmpDir
		if dir == "" {
			dir = filepath.Dir(w.args.IndexFile)
		}
		f, err := os.CreateTemp(dir, "btindex-offsets-*")
		if err != nil {
			return fmt.Errorf("btindex: create offsets spill: %w", err)
		}
		w.offsetsFile = f
		w.offsetsPath = f.Name()
		w.offsetsW = bufio.NewWriterSize(f, 1<<20)
	}
	binary.LittleEndian.PutUint64(w.offScratch[:], off)
	if _, err := w.offsetsW.Write(w.offScratch[:]); err != nil {
		return fmt.Errorf("btindex: write offsets spill: %w", err)
	}
	return nil
}

// AddKey records (key, offsetInDataFile) for the next ordinal. The
// caller MUST supply offsets in monotonically non-decreasing order
// (matching the natural order of a sequential `.kv` file read — see
// `btree_index.go:425-445` for the production iteration loop).
//
// Returns ErrAddKeyAfterBuild if called after Build, ErrTooManyKeys
// if called more than KeyCount times, ErrNonMonotonicOffset if the
// offset regresses, ErrEmptyKey if `key` is zero-length.
//
// The keep-as-node decision is internal: ordinal 0 is always kept
// (matches the b0 sentinel in BuildBtreeIndexWithDecompressor), and
// subsequent ordinals are kept iff `ordinal % M == 0`. The key bytes
// for retained nodes are COPIED (`common.Copy` in Erigon at
// `btree_index.go:282`); other keys are immediately discarded.
func (w *Writer) AddKey(key []byte, offsetInDataFile uint64) error {
	if w.built {
		return ErrAddKeyAfterBuild
	}
	if int(w.written) >= w.args.KeyCount {
		return ErrTooManyKeys
	}
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if w.written > 0 && offsetInDataFile < w.prevOff {
		return ErrNonMonotonicOffset
	}
	if offsetInDataFile > w.maxOffset {
		w.maxOffset = offsetInDataFile
	}

	// Replicate Erigon's `keep` logic byte-for-byte. The b0[] update
	// is unconditional (every key updates the sentinel) but only the
	// ordinal-0 branch can actually keep based on it; for ordinal > 0
	// the M-modulo rule overwrites.
	var keep bool
	if w.written == 0 {
		// At ordinal 0 b0 starts all-false, so this is always true.
		// We still mirror the b0 update for parity with Erigon's loop.
		if !w.b0[key[0]] {
			w.b0[key[0]] = true
			keep = true
		}
	} else {
		// Track b0 for fidelity even though the result is discarded
		// (Erigon's caller-side b0 update happens unconditionally
		// per `btree_index.go:428-430`).
		if !w.b0[key[0]] {
			w.b0[key[0]] = true
		}
		// Mirror `btree_index.go:241`: writer overwrites keep at
		// ordinal > 0.
		keep = w.written%uint64(w.args.M) == 0
	}

	if keep {
		// Erigon copies the key with `common.Copy` (`btree_index.go:282`)
		// since the caller's buffer is reused (`kv.Next(key[:0])`).
		// We do the same — callers may reuse their key buffer between
		// AddKey calls.
		cp := make([]byte, len(key))
		copy(cp, key)
		w.nodes = append(w.nodes, node{key: cp, di: w.written})
	}

	if err := w.appendOffset(offsetInDataFile); err != nil {
		return err
	}
	w.prevOff = offsetInDataFile
	w.written++
	return nil
}

// Build finalizes the index, writes it to disk, fsyncs, and atomically
// renames the tmp file to args.IndexFile. May only be called once.
//
// Mirrors `BtIndexWriter.Build` at btree_index.go:259-315. The
// `context.Context` is accepted for API symmetry with the rest of
// internal/erigon/* writers and to support future cancellation; the
// in-memory build is fast enough that we don't currently respect ctx
// inside the hot loop, but we check it once before fsync to provide
// at least a coarse cancellation point.
//
// If KeyCount == 0 (or no keys were added — same thing once we enforce
// the count-match invariant), the output is a zero-byte file: this is
// the legal empty-index case that Erigon's reader handles at
// `btree_index.go:489-491` (`if idx.size == 0 { return idx, nil }`).
func (w *Writer) Build(ctx context.Context) error {
	if w.built {
		return ErrAlreadyBuilt
	}
	if int(w.written) != w.args.KeyCount {
		return fmt.Errorf("%w: wrote %d, declared %d",
			ErrCountMismatch, w.written, w.args.KeyCount)
	}

	// Mark built up-front so any panic in the IO path doesn't leave
	// the writer reusable. Matches Erigon's eager `btw.built = true`
	// at `btree_index.go:300`.
	w.built = true

	tmpPath, f, err := createTempFile(w.args.IndexFile)
	if err != nil {
		return fmt.Errorf("create temp index file for %s: %w", w.args.IndexFile, err)
	}
	// Cleanup tmp on failure; on success rename consumes it.
	var succeeded bool
	defer func() {
		if !succeeded {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	bw := bufio.NewWriter(f)

	if w.written > 0 {
		// Build the embedded EliasFano. Mirrors `btree_index.go:274-293`.
		ef, efErr := eliasfano.New(uint64(w.written), w.maxOffset)
		if efErr != nil {
			return fmt.Errorf("alloc eliasfano: %w", efErr)
		}
		// Replay the spilled offsets in append order. A FIFO file
		// preserves the exact sequence AddKey saw, so EliasFano output is
		// byte-identical to the previous in-memory []uint64 path. The
		// spill file is guaranteed open here because written>0 implies at
		// least one appendOffset call.
		if err := w.offsetsW.Flush(); err != nil {
			return fmt.Errorf("flush offsets spill: %w", err)
		}
		if _, err := w.offsetsFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek offsets spill: %w", err)
		}
		or := bufio.NewReaderSize(w.offsetsFile, 1<<20)
		var offBuf [8]byte
		for i := uint64(0); i < w.written; i++ {
			if _, err := io.ReadFull(or, offBuf[:]); err != nil {
				return fmt.Errorf("read offsets spill (ordinal %d/%d): %w", i, w.written, err)
			}
			if addErr := ef.AddOffset(binary.LittleEndian.Uint64(offBuf[:])); addErr != nil {
				return fmt.Errorf("ef.AddOffset: %w", addErr)
			}
		}
		ef.Build()
		if err := ef.Serialize(bw); err != nil {
			return fmt.Errorf("write eliasfano: %w", err)
		}
		if err := encodeListNodes(w.nodes, bw); err != nil {
			return fmt.Errorf("write nodes: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, w.args.IndexFile); err != nil {
		return fmt.Errorf("rename tmp → final: %w", err)
	}
	succeeded = true
	return nil
}

// Close releases any in-memory state. Safe to call multiple times;
// safe to call before or after Build. Returns nil — no resources
// require error-returning teardown.
//
// Mirrors `BtIndexWriter.Close` at btree_index.go:333-343, minus the
// etl.Collector cleanup (we don't use one).
func (w *Writer) Close() error {
	w.nodes = nil
	w.offsetsW = nil
	if w.offsetsFile != nil {
		_ = w.offsetsFile.Close()
		w.offsetsFile = nil
	}
	if w.offsetsPath != "" {
		_ = os.Remove(w.offsetsPath)
		w.offsetsPath = ""
	}
	return nil
}
