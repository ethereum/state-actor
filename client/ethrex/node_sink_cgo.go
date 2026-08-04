//go:build cgo_ethrex

package ethrex

import (
	"bytes"
	"container/heap"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/linxGnu/grocksdb"

	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
)

// flushThresholdBytes is the WriteBatch flush threshold for the long-lived
// shared sinks during bulk import.
const flushThresholdBytes = 64 * 1024 * 1024

// workerFlushThresholdBytes is the smaller threshold for PER-WORKER sinks
// (Phase 0 and Stage B): a cleared WriteBatch retains its high-water C++
// buffer for the sink's lifetime, so N workers × 2 sinks × ~2× threshold is
// resident C heap — 16 MiB keeps 16 workers under ~1 GiB where 64 MiB would
// be ~4 GiB.
const workerFlushThresholdBytes = 16 * 1024 * 1024

// batchSink wraps a grocksdb.WriteBatch targeting a specific CF and a shared
// flush mechanism. It is used for the trie-node CFs (account_trie_nodes and
// storage_trie_nodes) where NodeSink callbacks write (path, value) pairs.
type batchSink struct {
	db        *ethrexDB
	cfIdx     int
	batch     *grocksdb.WriteBatch
	bytes     int
	threshold int
}

// newBatchSink constructs a batchSink for the given CF index.
// Caller must call Close() to flush any pending writes and free the WriteBatch.
func newBatchSink(db *ethrexDB, cfIdx int) *batchSink {
	return newBatchSinkWithThreshold(db, cfIdx, flushThresholdBytes)
}

// newBatchSinkWithThreshold is newBatchSink with an explicit flush threshold —
// per-worker sinks pass workerFlushThresholdBytes.
func newBatchSinkWithThreshold(db *ethrexDB, cfIdx, threshold int) *batchSink {
	return &batchSink{db: db, cfIdx: cfIdx, batch: grocksdb.NewWriteBatch(), threshold: threshold}
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
	if s.bytes < s.threshold {
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

// accountWorkItem is sent from Stage A (reader) to the worker pool (Stage B).
type accountWorkItem struct {
	seq      uint64
	addrHash common.Hash
	ent      entity
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
// ordered application. Storage rows are ALREADY WRITTEN (each worker owns a
// pair of storage batchSinks and streams the trie build directly into them),
// so a result carries only scalars — the reorder heap holds tiny payloads
// regardless of how large an account's storage was.
type accountResult struct {
	seq         uint64
	addrHash    common.Hash
	storageRoot common.Hash
	codeHash    common.Hash
	code        []byte // nil for EOAs; non-nil for contracts (used by Stage C writeCode)
	accountRLP  []byte
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
// storage trie build. value stays the raw 32 bytes end-to-end — the encoders
// trim it directly (EncodeStorageValueBytes32); no integer types involved.
type storageSlotKV struct {
	slotHash common.Hash
	value    common.Hash
}

// collectNonZeroSlots builds a sorted []storageSlotKV from ent.slots, skipping
// zero values. Returns nil if there are no non-zero slots. Slots are sorted by
// slotHash ascending, identical to the original single-pass writer.
func collectNonZeroSlots(ent entity) []storageSlotKV {
	if len(ent.slots) == 0 {
		return nil
	}
	kvs := make([]storageSlotKV, 0, len(ent.slots))
	var zero common.Hash
	for _, s := range ent.slots {
		if s.Value == zero {
			continue
		}
		kvs = append(kvs, storageSlotKV{
			slotHash: crypto.Keccak256Hash(s.Key[:]),
			value:    s.Value,
		})
	}
	if len(kvs) == 0 {
		return nil
	}
	slices.SortFunc(kvs, func(a, b storageSlotKV) int {
		return bytes.Compare(a.slotHash[:], b.slotHash[:])
	})
	return kvs
}

// buildStorageTrieInline builds the storage trie for ent, streaming every row
// directly into storageSink / storageFkvSink — no per-account buffering, so
// account size does not drive memory. Stage B workers call it with their own
// per-worker sink pair (the same pattern Phase 0 has always used for these
// two CFs); write order across accounts is therefore arbitrary, which the
// memtable path is indifferent to.
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

	var nibScratch [64]byte
	for _, e := range kvs {
		enc := ethrexinternal.EncodeStorageValueBytes32(e.value)
		if addErr := sb.AddLeaf(ethrexinternal.AppendNibbles(nibScratch[:0], e.slotHash[:]), enc); addErr != nil {
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
