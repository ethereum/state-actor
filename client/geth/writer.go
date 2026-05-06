package geth

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
)

// Writer writes state to a geth-compatible Pebble database using
// cockroachdb/pebble directly (no go-ethereum/ethdb/pebble wrapper, no
// goroutine pool). It mirrors the direct-cgo write pattern used by
// client/nethermind and client/besu, but with cockroach pebble in place of
// grocksdb because geth's on-disk format is Pebble.
//
// Hot-path writes (WriteAccount/WriteStorage/WriteCode and the trie-node
// callbacks driven by state_writer.go) accumulate into a single
// *pebble.Batch and flush at flushBytes. Cold-path callers (genesis block
// metadata, PathDB markers, rawdb.Open's freezer init) consume DB() — a
// thin pebbleKV adapter that satisfies ethdb.KeyValueStore over the same
// *pebble.DB.
//
// The Writer holds a single batch under a mutex. The hot path is
// single-goroutine in normal operation (state_writer.go drives it from one
// place); the mutex serialises that with mid-run FlushBatch calls issued
// from a different goroutine (target-size size-sampling). No worker pool
// — the slowness in the legacy go-ethereum/ethdb/pebble path traced to
// the batch worker mutex contention plus random write order, neither of
// which this design has.
type Writer struct {
	db        *pebble.DB
	kv        *pebbleKV // ethdb.KeyValueStore adapter; cold path only
	dbPath    string
	flushBytes int

	// batchMu serialises hot-path Put on `batch` against mid-run
	// FlushBatch / Flush. The hot path is single-goroutine; the mutex
	// exists for the cross-goroutine flush case.
	batchMu sync.Mutex
	batch   *pebble.Batch
	batchSz int

	accountBytes atomic.Uint64
	storageBytes atomic.Uint64
	codeBytes    atomic.Uint64
}

// defaultFlushBytes is the bulk-import batch flush threshold. 64 MiB
// matches client/besu/state_writer_cgo.go's phase1FlushBytes; large
// enough to amortise commit overhead, small enough to bound RAM.
const defaultFlushBytes = 64 * 1024 * 1024

// NewWriter opens the geth Pebble DB at dbPath using cockroachdb/pebble
// directly. batchSize and workers are accepted for backwards-compatibility
// with the generator.Writer signature; the new implementation derives its
// behaviour from a fixed ~64 MiB byte-budget flush instead.
//
// The DB is opened with bulk-import-friendly defaults: large MemTable,
// L0-compaction trigger raised, MaxConcurrentCompactions=4. WAL is kept
// enabled for the production DB so a crash doesn't lose the post-import
// metadata writes (Phase 1's scratch DB, opened separately in
// state_writer.go, disables WAL — that's what the speed delta buys).
func NewWriter(dbPath string, batchSize, workers int) (*Writer, error) {
	_ = batchSize // honoured implicitly via flushBytes; see comment above
	_ = workers   // worker pool removed; signature kept for compatibility

	db, err := pebble.Open(dbPath, prodPebbleOptions())
	if err != nil {
		return nil, fmt.Errorf("open pebble at %q: %w", dbPath, err)
	}

	w := &Writer{
		db:         db,
		kv:         newPebbleKV(db, &pebble.WriteOptions{Sync: false}),
		dbPath:     dbPath,
		flushBytes: defaultFlushBytes,
		batch:      db.NewBatch(),
	}
	return w, nil
}

// prodPebbleOptions returns pebble.Options tuned for state-actor's
// write-heavy bulk-import workload on the production geth DB. WAL stays
// enabled (durability for the metadata writes); other knobs are nudged
// from pebble defaults toward bulk-import friendliness:
//   - MemTableSize 128 MiB — fewer L0 flushes during a fresh import.
//   - L0CompactionThreshold 8 — postpones compactions until enough L0
//     files exist that a multi-file compaction is more efficient.
//   - MaxConcurrentCompactions 4 — keep up with import throughput.
//
// All values stay within Pebble's documented safe ranges and produce a DB
// that geth opens with no special flags.
func prodPebbleOptions() *pebble.Options {
	return &pebble.Options{
		MemTableSize:                64 * 1024 * 1024,
		MemTableStopWritesThreshold: 8,
		L0CompactionThreshold:       8,
		L0StopWritesThreshold:       24,
		MaxConcurrentCompactions:    func() int { return 4 },
	}
}

// DB returns an ethdb.KeyValueStore view of the underlying Pebble DB for
// cold-path callers (genesis block writer, rawdb.Write* helpers,
// rawdb.Open's freezer initialiser).
//
// The adapter does NOT own the *pebble.DB; closing the adapter is a no-op
// so that ad-hoc rawdb wrappers (e.g. fdb.Close in genesis_block.go) don't
// invalidate Writer's still-active handle.
func (w *Writer) DB() ethdb.KeyValueStore {
	return w.kv
}

// WriteAccount writes an account to the snapshot layer.
// addrHash is pre-computed keccak256(addr) to avoid redundant hashing.
func (w *Writer) WriteAccount(addr common.Address, addrHash common.Hash, acc *types.StateAccount, incarnation uint64) error {
	slimData := types.SlimAccountRLP(*acc)
	key := accountSnapshotKey(addrHash)
	return w.put(key, slimData, &w.accountBytes)
}

// WriteStorage writes a storage slot to the snapshot layer.
// addrHash and slotHash are pre-computed keccak256 hashes (addr and slot are unused in geth format).
func (w *Writer) WriteStorage(addr common.Address, addrHash common.Hash, slot common.Hash, slotHash common.Hash, value common.Hash) error {
	valueRLP, err := encodeStorageValue(value)
	if err != nil {
		return fmt.Errorf("encode storage value: %w", err)
	}
	key := storageSnapshotKey(addrHash, slotHash)
	return w.put(key, valueRLP, &w.storageBytes)
}

// WriteStorageRLP writes a storage slot with pre-encoded RLP value.
// Avoids double-encoding when the caller already has the RLP bytes.
func (w *Writer) WriteStorageRLP(addrHash common.Hash, slotHash common.Hash, valueRLP []byte) error {
	key := storageSnapshotKey(addrHash, slotHash)
	return w.put(key, valueRLP, &w.storageBytes)
}

// WriteRawStorage writes a storage slot using a pre-hashed trie key.
// The hashedSlot bypasses keccak256 and is used directly as the snapshot key.
func (w *Writer) WriteRawStorage(addr common.Address, incarnation uint64, hashedSlot, value common.Hash) error {
	addrHash := crypto.Keccak256Hash(addr[:])
	valueRLP, err := encodeStorageValue(value)
	if err != nil {
		return fmt.Errorf("encode storage value: %w", err)
	}
	key := storageSnapshotKey(addrHash, hashedSlot)
	return w.put(key, valueRLP, &w.storageBytes)
}

// WriteCode writes contract bytecode.
func (w *Writer) WriteCode(codeHash common.Hash, code []byte) error {
	key := codeKey(codeHash)
	return w.put(key, code, &w.codeBytes)
}

// SetStateRoot writes the snapshot root marker and PathDB initialization
// metadata. binaryTrie selects the namespace (raw vs "v"-prefixed) for the
// pathdb keys; geth's pathdb wraps its diskdb under the "v" prefix in
// bintrie mode (triedb/pathdb/database.go:168-170) so the writes have to
// match. When --genesis is provided, WriteGenesisBlock writes the same
// entries; doing it here too is idempotent and ensures non-genesis DBs
// also boot cleanly.
//
// Drains the in-flight bulk-import batch first so the metadata write lands
// after all snapshot/trie data — critical for SnapshotGenerator's "Done"
// flag to reflect a fully-populated state.
func (w *Writer) SetStateRoot(root common.Hash, binaryTrie bool) error {
	if err := w.flushBatch(true); err != nil {
		return fmt.Errorf("drain batch before SetStateRoot: %w", err)
	}
	if err := WritePathDBMetadata(w.kv, root, binaryTrie); err != nil {
		return fmt.Errorf("write pathdb metadata: %w", err)
	}
	return nil
}

// Flush commits all pending writes. Tearing down semantics matched the
// legacy goroutine-pool form for compatibility, but with no pool to drain
// the operation is just a final batch commit.
func (w *Writer) Flush() error {
	return w.flushBatch(true)
}

// FlushBatch commits the currently-buffered batch synchronously. Safe to
// call mid-generation (e.g. before a dirSize sample) — the hot path's
// next put() opens a fresh batch transparently.
func (w *Writer) FlushBatch() error {
	return w.flushBatch(true)
}

// Close closes the writer and the underlying pebble DB.
func (w *Writer) Close() error {
	if err := w.flushBatch(true); err != nil {
		// Best-effort flush before close — surface the error but still try
		// to close the DB so resources don't leak.
		_ = w.db.Close()
		return fmt.Errorf("final flush: %w", err)
	}
	return w.db.Close()
}

// Stats returns write statistics.
func (w *Writer) Stats() generator.WriterStats {
	return generator.WriterStats{
		AccountBytes: w.accountBytes.Load(),
		StorageBytes: w.storageBytes.Load(),
		CodeBytes:    w.codeBytes.Load(),
	}
}

// WriteGenesisBlockFull writes the genesis block with full genesis config.
func (w *Writer) WriteGenesisBlockFull(genesisConfig *genesis.Genesis, stateRoot common.Hash, binaryTrie bool) error {
	// The genesis block writer touches the DB through the kv adapter; flush
	// the hot-path batch first so its writes are visible to the metadata
	// path.
	if err := w.flushBatch(true); err != nil {
		return fmt.Errorf("drain batch before genesis block: %w", err)
	}
	ancientDir := filepath.Join(w.dbPath, "ancient")
	_, err := WriteGenesisBlock(w.kv, genesisConfig, stateRoot, binaryTrie, ancientDir)
	return err
}

// PutTrieNode writes a single trie node to the in-flight bulk-import
// batch, accounting it under the storage-bytes counter for now (trie
// nodes are not separately metered today). Exposed for state_writer.go.
//
// The accounting is intentionally rough — the LiveStats progress bar
// cares about gross order-of-magnitude bytes-on-disk, not category
// precision. A separate trieNodeBytes counter can be added when callers
// need it.
func (w *Writer) PutTrieNode(key, blob []byte) error {
	return w.put(key, blob, &w.storageBytes)
}

// put is the shared hot-path entry. Single-goroutine in normal operation;
// batchMu protects against mid-run FlushBatch from a sampler goroutine.
func (w *Writer) put(key, value []byte, counter *atomic.Uint64) error {
	w.batchMu.Lock()
	if err := w.batch.Set(key, value, nil); err != nil {
		w.batchMu.Unlock()
		return fmt.Errorf("pebble batch set: %w", err)
	}
	w.batchSz += len(key) + len(value)
	counter.Add(uint64(len(key) + len(value)))
	full := w.batchSz >= w.flushBytes
	w.batchMu.Unlock()
	if full {
		return w.flushBatch(false)
	}
	return nil
}

// flushBatch commits the current batch and rotates in a new one. sync
// controls whether pebble.WriteOptions.Sync=true is set; the periodic
// auto-flush from put() uses sync=false (durability handled by the next
// sync flush or Close), while explicit Flush/FlushBatch/SetStateRoot use
// sync=true to guarantee the write hits stable storage.
func (w *Writer) flushBatch(sync bool) error {
	w.batchMu.Lock()
	if w.batchSz == 0 {
		w.batchMu.Unlock()
		return nil
	}
	old := w.batch
	w.batch = w.db.NewBatch()
	w.batchSz = 0
	w.batchMu.Unlock()

	opts := &pebble.WriteOptions{Sync: sync}
	if err := old.Commit(opts); err != nil {
		return fmt.Errorf("pebble batch commit: %w", err)
	}
	return nil
}

// --- Key encoding functions matching geth's rawdb schema ---

var (
	snapshotAccountPrefix = []byte("a")
	snapshotStoragePrefix = []byte("o")
	codePrefix            = []byte("c")
)

func accountSnapshotKey(hash common.Hash) []byte {
	return append(snapshotAccountPrefix, hash.Bytes()...)
}

func storageSnapshotKey(accountHash, storageHash common.Hash) []byte {
	buf := make([]byte, len(snapshotStoragePrefix)+common.HashLength+common.HashLength)
	n := copy(buf, snapshotStoragePrefix)
	n += copy(buf[n:], accountHash.Bytes())
	copy(buf[n:], storageHash.Bytes())
	return buf
}

func codeKey(hash common.Hash) []byte {
	return append(codePrefix, hash.Bytes()...)
}

func encodeStorageValue(value common.Hash) ([]byte, error) {
	trimmed := trimLeftZeroes(value[:])
	if len(trimmed) == 0 {
		return nil, nil
	}
	encoded, err := rlp.EncodeToBytes(trimmed)
	if err != nil {
		return nil, fmt.Errorf("failed to RLP-encode storage value %x: %w", value, err)
	}
	return encoded, nil
}

func trimLeftZeroes(s []byte) []byte {
	for i, v := range s {
		if v != 0 {
			return s[i:]
		}
	}
	return nil
}

