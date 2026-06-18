//go:build cgo_ethrex

package ethrex

import (
	"bytes"
	"container/heap"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/linxGnu/grocksdb"

	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
)

// flushThresholdBytes is the WriteBatch flush threshold during bulk import.
const flushThresholdBytes = 64 * 1024 * 1024

// batchSink wraps a grocksdb.WriteBatch targeting a specific CF and a shared
// flush mechanism. It is used for the trie-node CFs (account_trie_nodes and
// storage_trie_nodes) where NodeSink callbacks write (path, value) pairs.
type batchSink struct {
	db    *ethrexDB
	cfIdx int
	batch *grocksdb.WriteBatch
	bytes int
}

// newBatchSink constructs a batchSink for the given CF index.
// Caller must call Close() to flush any pending writes and free the WriteBatch.
func newBatchSink(db *ethrexDB, cfIdx int) *batchSink {
	return &batchSink{db: db, cfIdx: cfIdx, batch: grocksdb.NewWriteBatch()}
}

// put adds a key/value to the batch and triggers a flush if the threshold
// is exceeded.
func (s *batchSink) put(key, value []byte) error {
	s.batch.PutCF(s.db.cfs[s.cfIdx], key, value)
	s.bytes += len(key) + len(value)
	return s.maybeFlush()
}

// maybeFlush flushes the WriteBatch if the byte threshold is exceeded.
func (s *batchSink) maybeFlush() error {
	if s.bytes < flushThresholdBytes {
		return nil
	}
	return s.flushAsync()
}

func (s *batchSink) flushAsync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.DisableWAL(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("ethrex: flush batch (cf=%d): %w", s.cfIdx, err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}

func (s *batchSink) flushSync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.SetSync(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("ethrex: sync-flush batch (cf=%d): %w", s.cfIdx, err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}

// Close drains pending writes and releases the WriteBatch. Idempotent.
func (s *batchSink) Close() error {
	if s.batch == nil {
		return nil
	}
	defer func() {
		s.batch.Destroy()
		s.batch = nil
	}()
	if s.bytes == 0 {
		return nil
	}
	return s.flushSync()
}

// ---------------------------------------------------------------------------
// Parallel pipeline types (Phase 2 of writeState)
// ---------------------------------------------------------------------------

// parallelStorageSlotThreshold is the per-account slot count above which storage
// trie building is NOT dispatched to a worker. Accounts exceeding this threshold
// are processed inline (single-threaded) by Stage C to bound worst-case memory.
const parallelStorageSlotThreshold = 1 << 16 // 65536 slots ≈ 64 MiB buffered

// storageRow is one (path, value) pair captured from the buffering storage sink.
// path is already address-prefixed (as PrefixedSink would emit), so Stage C can
// apply isLeafFullPathHelper and route directly without re-prefixing.
type storageRow struct {
	path  []byte
	value []byte
}

// bufferingStorageSink returns a NodeSink that appends every (path, value) pair
// to *rows instead of writing RocksDB. The caller wraps the raw Builder sink
// with ethrexinternal.PrefixedSink first so that the captured paths are already
// address-prefixed.
func bufferingStorageSink(rows *[]storageRow) ethrexinternal.NodeSink {
	return func(path, value []byte) error {
		p := make([]byte, len(path))
		copy(p, path)
		v := make([]byte, len(value))
		copy(v, value)
		*rows = append(*rows, storageRow{path: p, value: v})
		return nil
	}
}

// accountWorkItem is sent from Stage A (reader) to the worker pool (Stage B).
type accountWorkItem struct {
	seq      uint64
	addrHash common.Hash
	ent      entity
	// bigAccount is true when len(ent.slots) > parallelStorageSlotThreshold.
	// Workers emit an accountResult with bigAccount=true and no storageRows;
	// Stage C builds their storage inline at their seq position.
	bigAccount bool
	// hasPreAllocRoot is true when preAllocStorageRoots[addrHash] exists.
	// Workers use preAllocRoot directly — no storage trie build needed.
	hasPreAllocRoot bool
	preAllocRoot    common.Hash
}

// accountStatDelta holds the per-account contribution to stats, computed
// by either a worker (Stage B) or inline in Stage C.
type accountStatDelta struct {
	StorageSlotsCreated uint64
	StorageBytes        uint64
	AccountBytes        uint64
	CodeBytes           uint64
	IsContract          bool // true → ContractsCreated++, false → AccountsCreated++
}

// accountResult is produced by Stage B workers and sent to Stage C for
// ordered application. Fields relevant only to big accounts are labelled.
type accountResult struct {
	seq      uint64
	addrHash common.Hash
	// bigAccount is true when storage must be built inline by Stage C.
	bigAccount bool
	// bigEnt holds the entity for big accounts so Stage C can build storage inline.
	bigEnt entity
	// storageRows holds address-prefixed (path, value) rows for normal accounts.
	storageRows []storageRow
	storageRoot common.Hash
	codeHash    common.Hash
	code        []byte // nil for EOAs; non-nil for contracts (used by Stage C writeCode)
	accountRLP  []byte // empty for big accounts; Stage C recomputes after inline build
	stats       accountStatDelta
	// buildErr carries a storage-build error from a worker.
	buildErr error
}

// ---------------------------------------------------------------------------
// Min-heap for reorder buffer (Stage C)
// ---------------------------------------------------------------------------

// resultHeap is a min-heap of *accountResult ordered by seq (ascending).
type resultHeap []*accountResult

func (h resultHeap) Len() int            { return len(h) }
func (h resultHeap) Less(i, j int) bool  { return h[i].seq < h[j].seq }
func (h resultHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *resultHeap) Push(x interface{}) { *h = append(*h, x.(*accountResult)) }
func (h *resultHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

// newResultHeap returns an initialised empty heap.
func newResultHeap() *resultHeap {
	h := &resultHeap{}
	heap.Init(h)
	return h
}

// ---------------------------------------------------------------------------
// Shared storage-building helpers used by Stage B workers and Stage C inline
// ---------------------------------------------------------------------------

// isLeafFullPathHelper reports whether path is a leaf full-path row (key ends
// in the LeafFlag nibble). These are the only rows the Builder emits whose key
// ends in the leaf-flag nibble; branch/extension/leaf-node-RLP rows and the
// empty-trie sentinel never do. Used by both Stage B workers and Stage C inline
// routing in writeState.
func isLeafFullPathHelper(path []byte) bool {
	return len(path) > 0 && path[len(path)-1] == ethrexinternal.LeafFlag
}

// storageSlotKV is a (slotHash, value) pair used when sorting slots for the
// storage trie build.
type storageSlotKV struct {
	slotHash common.Hash
	value    *uint256.Int
}

// collectNonZeroSlots builds a sorted []storageSlotKV from ent.slots, skipping
// zero values. Returns nil if there are no non-zero slots. Slots are sorted by
// slotHash ascending, identical to the original single-pass writer.
func collectNonZeroSlots(ent entity) []storageSlotKV {
	if len(ent.slots) == 0 {
		return nil
	}
	kvs := make([]storageSlotKV, 0, len(ent.slots))
	for slotKey, slotVal := range ent.slots {
		if slotVal.IsZero() {
			continue
		}
		kvs = append(kvs, storageSlotKV{
			slotHash: crypto.Keccak256Hash(slotKey[:]),
			value:    slotVal.Clone(),
		})
	}
	if len(kvs) == 0 {
		return nil
	}
	sort.Slice(kvs, func(i, j int) bool {
		return bytes.Compare(kvs[i].slotHash[:], kvs[j].slotHash[:]) < 0
	})
	return kvs
}

// buildStorageTrieBuffered builds the storage trie for ent entirely in memory,
// returning the address-prefixed captured rows and the storage root.
// It does NOT touch RocksDB. Used by Stage B workers.
func buildStorageTrieBuffered(
	addrHash common.Hash,
	ent entity,
	emptyTrieHash common.Hash,
) (rows []storageRow, storageRoot common.Hash, storageSlotsCreated, storageBytes uint64, err error) {
	kvs := collectNonZeroSlots(ent)
	if kvs == nil {
		return nil, emptyTrieHash, 0, 0, nil
	}

	var capturedRows []storageRow
	captureSink := bufferingStorageSink(&capturedRows)
	prefixedSink := ethrexinternal.PrefixedSink(addrHash, captureSink)
	sb := ethrexinternal.NewBuilder(prefixedSink)

	for _, e := range kvs {
		enc := ethrexinternal.EncodeStorageValue(e.value)
		if addErr := sb.AddLeaf(ethrexinternal.BytesToNibbles(e.slotHash[:]), enc); addErr != nil {
			return nil, emptyTrieHash, 0, 0, fmt.Errorf("ethrex: storage leaf: %w", addErr)
		}
		storageSlotsCreated++
		storageBytes += uint64(len(enc))
	}

	root, rootErr := sb.Root()
	if rootErr != nil {
		return nil, emptyTrieHash, 0, 0, fmt.Errorf("ethrex: storage root: %w", rootErr)
	}
	return capturedRows, root, storageSlotsCreated, storageBytes, nil
}

// buildStorageTrieInline builds the storage trie for ent and writes rows
// directly to storageSink / storageFkvSink (no buffering). Used by Stage C for
// big accounts.
func buildStorageTrieInline(
	addrHash common.Hash,
	ent entity,
	emptyTrieHash common.Hash,
	storageSink *batchSink,
	storageFkvSink *batchSink,
) (storageRoot common.Hash, storageSlotsCreated, storageBytes uint64, err error) {
	kvs := collectNonZeroSlots(ent)
	if kvs == nil {
		return emptyTrieHash, 0, 0, nil
	}

	inlineSink := ethrexinternal.NodeSink(func(path, value []byte) error {
		if isLeafFullPathHelper(path) {
			return storageFkvSink.put(path, value)
		}
		return storageSink.put(path, value)
	})
	prefixedSink := ethrexinternal.PrefixedSink(addrHash, inlineSink)
	sb := ethrexinternal.NewBuilder(prefixedSink)

	for _, e := range kvs {
		enc := ethrexinternal.EncodeStorageValue(e.value)
		if addErr := sb.AddLeaf(ethrexinternal.BytesToNibbles(e.slotHash[:]), enc); addErr != nil {
			return emptyTrieHash, 0, 0, fmt.Errorf("ethrex: storage leaf: %w", addErr)
		}
		storageSlotsCreated++
		storageBytes += uint64(len(enc))
	}

	root, rootErr := sb.Root()
	if rootErr != nil {
		return emptyTrieHash, 0, 0, fmt.Errorf("ethrex: storage root: %w", rootErr)
	}
	return root, storageSlotsCreated, storageBytes, nil
}
