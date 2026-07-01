package snap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/spaolacci/murmur3"

	"github.com/ethereum/state-actor/internal/erigon"
	"github.com/ethereum/state-actor/internal/erigon/btindex"
	"github.com/ethereum/state-actor/internal/erigon/existence"
	"github.com/ethereum/state-actor/internal/erigon/recsplit"
	"github.com/ethereum/state-actor/internal/erigon/seg"
)

// Writer composes seg + btindex + existence into Erigon's per-domain
// snapshot file set. Construct via NewWriter; emit a domain via
// WriteDomain; finalize via Close.
//
// Re-creating a Writer over an existing datadir is supported: NewWriter
// verifies salt/erigondb invariants match what's already on disk and
// returns an error on mismatch (so we never silently emit a file set
// against a different salt than the existence filters were built with).
type Writer struct {
	datadir   string
	settings  Settings
	closed    bool
}

// NewWriter validates the on-disk salt + erigondb.toml against
// Settings, creates them if absent, ensures the snapshot directory
// layout exists, and returns a Writer ready to emit domains.
//
// Defaults applied to s if zero:
//   - StepSize:          erigon.StepSize (390_625)
//   - StepsInFrozenFile: erigon.StepsInFrozenFile (256)
//   - SnapshotVersion:   erigon.SnapshotFormatVersion ("v1.0")
//   - Salt:              DeriveSaltFromSeed(s.Seed) if both Salt==0
//                        and no salt-state.txt exists yet on disk
//   - Accessors[d]:      DefaultAccessorMask(d) per Verifier B's
//                        correction (per-domain mix)
func NewWriter(datadir string, s Settings) (*Writer, error) {
	if datadir == "" {
		return nil, errors.New("snap.NewWriter: datadir is required")
	}
	if s.StepSize == 0 {
		s.StepSize = erigon.StepSize
	}
	if s.StepsInFrozenFile == 0 {
		s.StepsInFrozenFile = erigon.StepsInFrozenFile
	}
	if s.SnapshotVersion == "" {
		s.SnapshotVersion = erigon.SnapshotFormatVersion
	}
	if s.Accessors == nil {
		s.Accessors = make(map[Domain]AccessorMask, 4)
	}
	for _, d := range []Domain{DomainAccounts, DomainStorage, DomainCode, DomainCommitment} {
		if _, ok := s.Accessors[d]; !ok {
			s.Accessors[d] = DefaultAccessorMask(d)
		}
	}

	if err := EnsureSnapshotLayout(datadir); err != nil {
		return nil, err
	}

	// Salt: if on-disk salt-state.txt exists, it wins (idempotency).
	// Otherwise derive from seed (or use the caller-provided salt) and
	// persist.
	if existingSalt, err := ReadSalt(datadir); err == nil {
		if s.Salt != 0 && s.Salt != existingSalt {
			return nil, fmt.Errorf("snap.NewWriter: salt mismatch: settings=%d, on-disk=%d",
				s.Salt, existingSalt)
		}
		s.Salt = existingSalt
	} else if errors.Is(err, os.ErrNotExist) {
		if s.Salt == 0 {
			s.Salt = DeriveSaltFromSeed(s.Seed)
		}
		if err := WriteSalt(datadir, s.Salt); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("snap.NewWriter: read salt-state.txt: %w", err)
	}

	// erigondb.toml: same idempotency contract — error on mismatch,
	// write if absent.
	if err := ensureErigonDBSettings(datadir, s.StepSize, s.StepsInFrozenFile); err != nil {
		return nil, err
	}

	return &Writer{datadir: datadir, settings: s}, nil
}

func ensureErigonDBSettings(datadir string, want, wantFrozen uint64) error {
	path := ErigonDBSettingsPath(datadir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return WriteErigonDBSettings(datadir, want, wantFrozen)
	}
	if err != nil {
		return fmt.Errorf("snap.NewWriter: read erigondb.toml: %w", err)
	}
	// Cheap presence check — Erigon's parser is lenient on whitespace,
	// so we look for the literal "step_size = <want>" and
	// "steps_in_frozen_file = <wantFrozen>" substrings. A real
	// mismatch returns an error rather than silently overwriting.
	wantStep := fmt.Sprintf("step_size = %d", want)
	wantSteps := fmt.Sprintf("steps_in_frozen_file = %d", wantFrozen)
	body := string(raw)
	if !contains(body, wantStep) || !contains(body, wantSteps) {
		return fmt.Errorf("snap.NewWriter: erigondb.toml mismatch: want %q + %q, got %q",
			wantStep, wantSteps, body)
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Salt returns the snapshot salt that's persisted at
// <datadir>/snapshots/salt-state.txt.
func (w *Writer) Salt() uint32 { return w.settings.Salt }

// WriteDomain emits the per-domain file set (.kv data + accessors) for
// (d, r). entries MUST yield ascending keys; behaviour is undefined
// otherwise. The data file is written first via seg.Compressor, then
// re-iterated via seg.Decompressor to feed the accessor builders —
// matching the two-pass pattern from Erigon's
// simple_accessor_builder.go:194-216 (Verifier B's Correction 2).
//
// keyCount is required up-front: the bloom-filter sizing
// (existence.NewFilterBuilder) and B+tree node count depend on it.
// If the caller doesn't know the count statically, materialize into a
// slice first.
//
// Output paths (under <datadir>/snapshots/domain/):
//
//	v1.0-accounts.0-256.kv
//	v1.0-accounts.0-256.bt    (AccessorBTree)
//	v1.0-accounts.0-256.kvi   (AccessorHashMap)   — commitment only
//	v1.0-accounts.0-256.kvei  (AccessorExistence) — all domains by default
func (w *Writer) WriteDomain(ctx context.Context, d Domain, r StepRange, keyCount uint64, entries func(yield func(DomainEntry) bool)) error {
	if w.closed {
		return errors.New("snap.Writer: Closed")
	}
	if r.From >= r.To {
		return fmt.Errorf("snap.WriteDomain: invalid StepRange [%d, %d)", r.From, r.To)
	}

	domainDir := DomainDir(w.datadir)
	tmpDir := domainDir // seg.Compressor writes its own .tmp files under tmpDir
	dataPath := BuildDataFilename(domainDir, w.settings.SnapshotVersion, d, r)

	// Pass 1: stream (k, v) into seg.Compressor.
	cfg := seg.DefaultConfig()
	comp, err := seg.NewCompressor(dataPath, tmpDir, cfg)
	if err != nil {
		return fmt.Errorf("snap.WriteDomain: seg.NewCompressor: %w", err)
	}
	// `entries` is a push-style iterator — invoke it with our consumer.
	var addErr error
	entries(func(e DomainEntry) bool {
		if err := comp.AddWord(e.Key); err != nil {
			addErr = fmt.Errorf("AddWord(key): %w", err)
			return false
		}
		if err := comp.AddWord(e.Value); err != nil {
			addErr = fmt.Errorf("AddWord(value): %w", err)
			return false
		}
		return true
	})
	if addErr != nil {
		_ = comp.Close()
		return fmt.Errorf("snap.WriteDomain: pass-1: %w", addErr)
	}
	if err := comp.Compress(); err != nil {
		_ = comp.Close()
		return fmt.Errorf("snap.WriteDomain: seg.Compress: %w", err)
	}
	if err := comp.Close(); err != nil {
		return fmt.Errorf("snap.WriteDomain: seg.Close: %w", err)
	}

	// Pass 2: re-open the .kv, iterate (key, val, keyOff, valOff), feed
	// the accessor builders.
	mask := w.settings.Accessors[d]
	dec, err := seg.NewDecompressor(dataPath)
	if err != nil {
		return fmt.Errorf("snap.WriteDomain: seg.NewDecompressor: %w", err)
	}
	defer dec.Close()

	var bt *btindex.Writer
	if mask.Has(AccessorBTree) {
		btPath := BuildBTreeFilename(domainDir, w.settings.SnapshotVersion, d, r)
		bt, err = btindex.New(btindex.Args{
			KeyCount:  int(keyCount),
			TmpDir:    tmpDir,
			IndexFile: btPath,
		})
		if err != nil {
			return fmt.Errorf("snap.WriteDomain: btindex.New: %w", err)
		}
		// Release the offsets spill file on every exit path. Close is
		// idempotent, so the explicit Close after a successful Build is a
		// no-op; this defer is what prevents a multi-GB spill-file leak
		// when the iterate loop or Build below returns an error.
		defer bt.Close()
	}

	var exist *existence.FilterBuilder
	if mask.Has(AccessorExistence) {
		exPath := BuildExistenceFilename(domainDir, w.settings.SnapshotVersion, d, r)
		exist, err = existence.NewFilterBuilder(keyCount, exPath, false)
		if err != nil {
			return fmt.Errorf("snap.WriteDomain: existence.NewFilterBuilder: %w", err)
		}
	}

	// AccessorHashMap (.kvi via recsplit) — commitment-domain default.
	// Recsplit may need to retry on hash collisions; for v1 we surface
	// the collision as an error rather than auto-retry (caller can
	// bump Settings.Salt and re-invoke).
	var hashm *recsplit.Writer
	if mask.Has(AccessorHashMap) {
		hmPath := BuildHashMapFilename(domainDir, w.settings.SnapshotVersion, d, r)
		saltCopy := w.settings.Salt // recsplit needs a *uint32 to mutate on collision
		hashm, err = recsplit.New(recsplit.Args{
			KeyCount:   int(keyCount),
			BucketSize: 100,
			Salt:       &saltCopy,
			LeafSize:   8,
			TmpDir:     tmpDir,
			IndexFile:  hmPath,
		})
		if err != nil {
			return fmt.Errorf("snap.WriteDomain: recsplit.New: %w", err)
		}
		// recsplit.New opens its bucket streamsort (a Pebble dir) eagerly;
		// this defer removes it on every exit path. Idempotent with the
		// explicit Close after a successful Build.
		defer hashm.Close()
	}

	// Salt-prehash for the existence filter (per Verifier B's note in
	// the plan: caller is responsible for the salt pre-hash):
	//   hash := murmur3.Sum128WithSeed(key, salt)
	// We use the first uint64 of the 128-bit hash because the existence
	// filter's AddHash signature accepts a uint64. Erigon's writer uses
	// the same lower 64 bits.
	saltBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(saltBytes, w.settings.Salt)

	for entry, err := range dec.Iterate(ctx) {
		if err != nil {
			return fmt.Errorf("snap.WriteDomain: decompressor iterate: %w", err)
		}
		if bt != nil {
			// MUST be KeyOffset, not ValueOffset: upstream's BtIndex.dataLookup
			// (db/datastruct/btindex/btree_index.go:531) does
			// `g.Reset(offset); g.Next(nil) /*=key*/; g.Next(nil) /*=value*/`
			// — so `offset` must point at the key's length-prefix, NOT at
			// the value. Upstream's reference builder confirms (same file
			// line 432): `pos` is captured BEFORE `kv.Next(key[:0])`.
			// Using ValueOffset here positions the Huffman cursor mid-entry
			// and walks dataP past EOF on the first system-contract lookup
			// of block 0 (manifested as
			// `panic: index out of range [N] with length N` in
			// exec3_serial.go:349).
			if err := bt.AddKey(entry.Key, entry.KeyOffset); err != nil {
				return fmt.Errorf("snap.WriteDomain: btindex.AddKey: %w", err)
			}
		}
		if hashm != nil {
			if err := hashm.AddKey(entry.Key, entry.KeyOffset); err != nil {
				return fmt.Errorf("snap.WriteDomain: recsplit.AddKey: %w", err)
			}
		}
		if exist != nil {
			lo, _ := murmur3.Sum128WithSeed(entry.Key, w.settings.Salt)
			if err := exist.AddHash(lo); err != nil {
				return fmt.Errorf("snap.WriteDomain: existence.AddHash: %w", err)
			}
		}
	}

	if bt != nil {
		if err := bt.Build(ctx); err != nil {
			return fmt.Errorf("snap.WriteDomain: btindex.Build: %w", err)
		}
		if err := bt.Close(); err != nil {
			return fmt.Errorf("snap.WriteDomain: btindex.Close: %w", err)
		}
	}
	if hashm != nil {
		if err := hashm.Build(ctx); err != nil {
			if hashm.Collision() {
				return fmt.Errorf("snap.WriteDomain: recsplit hash collision — bump Settings.Salt and retry: %w", err)
			}
			return fmt.Errorf("snap.WriteDomain: recsplit.Build: %w", err)
		}
		if err := hashm.Close(); err != nil {
			return fmt.Errorf("snap.WriteDomain: recsplit.Close: %w", err)
		}
	}
	if exist != nil {
		if err := exist.Build(); err != nil {
			return fmt.Errorf("snap.WriteDomain: existence.Build: %w", err)
		}
	}
	return nil
}

// Close marks the Writer as no-longer-usable. Snapshot files are
// already fsynced at WriteDomain return; Close is reserved for future
// metadata-flush hooks.
func (w *Writer) Close() error {
	w.closed = true
	return nil
}
