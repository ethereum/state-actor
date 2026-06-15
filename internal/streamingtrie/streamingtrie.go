// Package streamingtrie computes a storage-trie root from a streaming
// per-entity slot iter, in bounded RAM regardless of slot count.
//
// Pipeline:
//
//  1. Drain (slotKey, value) into a streamsort.Store keyed by
//     keccak(slotKey). Zero-valued slots are skipped (canonical).
//  2. Iterate sorted; for each entry call the Sink (per-client DB row
//     writes), then feed (keyHash, trim+RLP(value)) to the HashBuilder.
//  3. Return HashBuilder.Root().
//
// RAM is bounded by streamsort.MemTableSize. Disk usage during the
// drain is O(slot_count × 96 B) in the temp Pebble dir.
//
// Producers that are pure functions of their inputs may be passed to
// StorageRoot multiple times and produce identical roots.
package streamingtrie

import (
	"encoding/binary"
	"fmt"
	"iter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/internal/streamsort"
)

// HashBuilder is the per-client streaming MPT builder contract.
// AddLeaf is called in keccak-ascending order with the 32-byte keyHash
// (already keccak'd by StorageRoot) and the RLP-encoded value bytes.
// Root is called once after iteration completes.
type HashBuilder interface {
	AddLeaf(keyHash common.Hash, valueRLP []byte) error
	Root() (common.Hash, error)
}

// Sink is invoked once per sorted (keyHash, rawKey, value) triple
// before the trie-leaf is appended. Non-nil error short-circuits.
type Sink func(keyHash, rawKey, value common.Hash) error

// StorageRoot drains storage into a streamsort.Store, walks the sorted
// output to drive Sink and HashBuilder in lockstep, and returns the
// storage trie root.
//
// workDir is passed through to streamsort.New (empty → os.TempDir()).
// storage MUST yield deterministically. hb MUST be freshly constructed.
// sink MAY be nil (root-only path).
//
// Equivalent to: Drain → IterateRoot → Close. Use the split form when
// the drain phase should run in a different goroutine from the
// iterate-with-sink phase (e.g. to parallelise drain across entities
// while serialising the write phase on a single DB-writer lock).
func StorageRoot(
	workDir string,
	storage iter.Seq2[common.Hash, common.Hash],
	hb HashBuilder,
	sink Sink,
) (common.Hash, error) {
	if hb == nil {
		return common.Hash{}, fmt.Errorf("streamingtrie: nil HashBuilder")
	}
	d, err := Drain(workDir, storage)
	if err != nil {
		return common.Hash{}, err
	}
	defer d.Close()
	return d.IterateRoot(hb, sink)
}

// Drained is a finished drain-phase result — a sorted on-disk store of
// (keyHash, rawKey||value) entries ready to be replayed in keccak-
// ascending order via IterateRoot. The caller MUST Close it once the
// iterate phase is done.
//
// A Drained is owned by a single goroutine at a time; the drain
// goroutine may hand it off to a writer goroutine over a channel.
type Drained struct {
	store *streamsort.Store
}

// Drain hashes every (rawKey, value) pair from storage, skips zero
// values, and writes (keccak(rawKey), rawKey||value) to a temp
// streamsort.Store. The store's Iterate is called later via
// IterateRoot. The returned Drained owns the streamsort and MUST be
// closed by the caller.
//
// nil storage is allowed and returns an empty Drained (IterateRoot
// will yield the empty-trie root).
func Drain(
	workDir string,
	storage iter.Seq2[common.Hash, common.Hash],
) (*Drained, error) {
	s, err := streamsort.New(workDir)
	if err != nil {
		return nil, fmt.Errorf("streamingtrie: streamsort.New: %w", err)
	}
	if storage == nil {
		return &Drained{store: s}, nil
	}
	// Store layout: key=keccak(rawKey) (32 B), value=rawKey||value (64 B).
	var putErr error
	storage(func(k, v common.Hash) bool {
		if v == (common.Hash{}) {
			return true
		}
		keyHash := crypto.Keccak256Hash(k[:])
		var combined [64]byte
		copy(combined[0:32], k[:])
		copy(combined[32:64], v[:])
		if err := s.Put(keyHash[:], combined[:]); err != nil {
			putErr = fmt.Errorf("streamingtrie: drain Put: %w", err)
			return false
		}
		return true
	})
	if putErr != nil {
		s.Close()
		return nil, putErr
	}
	return &Drained{store: s}, nil
}

// IterateRoot walks the drained store in keccak-ascending order,
// invoking sink (if non-nil) and HashBuilder.AddLeaf in lockstep,
// then returns the storage trie root. Callable exactly once per
// Drained; closing is the caller's responsibility.
func (d *Drained) IterateRoot(hb HashBuilder, sink Sink) (common.Hash, error) {
	if hb == nil {
		return common.Hash{}, fmt.Errorf("streamingtrie: nil HashBuilder")
	}
	if err := d.store.Iterate(func(keyHashB, combinedB []byte) error {
		var keyHash, rawKey, value common.Hash
		copy(keyHash[:], keyHashB)
		copy(rawKey[:], combinedB[0:32])
		copy(value[:], combinedB[32:64])

		if sink != nil {
			if err := sink(keyHash, rawKey, value); err != nil {
				return fmt.Errorf("streamingtrie: sink: %w", err)
			}
		}

		valBytes := value[:]
		for len(valBytes) > 0 && valBytes[0] == 0 {
			valBytes = valBytes[1:]
		}
		valRLP, err := rlp.EncodeToBytes(valBytes)
		if err != nil {
			return fmt.Errorf("streamingtrie: rlp encode value: %w", err)
		}
		if err := hb.AddLeaf(keyHash, valRLP); err != nil {
			return fmt.Errorf("streamingtrie: AddLeaf: %w", err)
		}
		return nil
	}); err != nil {
		return common.Hash{}, err
	}
	root, err := hb.Root()
	if err != nil {
		return common.Hash{}, fmt.Errorf("streamingtrie: Root: %w", err)
	}
	return root, nil
}

// Close releases the underlying streamsort store. Idempotent.
func (d *Drained) Close() {
	if d == nil || d.store == nil {
		return
	}
	_ = d.store.Close()
	d.store = nil
}

func uint64SlotKey(slot uint64) common.Hash {
	var h common.Hash
	binary.BigEndian.PutUint64(h[24:32], slot)
	return h
}
