//go:build cgo_nimbus

package nimbus

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/entitygen"
	"github.com/ethereum/state-actor/internal/ethrex"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

// flushThresholdBytes is the WriteBatch flush threshold for the long-lived
// shared sinks (account trie, code); workerFlushThresholdBytes the smaller
// one for per-worker storage sinks — a cleared WriteBatch keeps its
// high-water C buffer, so N workers × ~2× threshold is resident C heap.
const (
	flushThresholdBytes       = 64 * 1024 * 1024
	workerFlushThresholdBytes = 16 * 1024 * 1024
)

// batchSink accumulates rows for one CF in a grocksdb.WriteBatch and flushes
// (WAL disabled) once it grows past its threshold.
type batchSink struct {
	db        *nimbusDB
	cfIdx     int
	batch     *grocksdb.WriteBatch
	bytes     int
	threshold int
}

func newBatchSink(db *nimbusDB, cfIdx int) *batchSink {
	return newBatchSinkWithThreshold(db, cfIdx, flushThresholdBytes)
}

func newBatchSinkWithThreshold(db *nimbusDB, cfIdx, threshold int) *batchSink {
	return &batchSink{db: db, cfIdx: cfIdx, batch: grocksdb.NewWriteBatch(), threshold: threshold}
}

// put copies key/value into the batch (grocksdb copies into C memory
// immediately, so borrowed slices are fine) and flushes past the threshold.
func (s *batchSink) put(key, value []byte) error {
	s.batch.PutCF(s.db.cfs[s.cfIdx], key, value)
	s.bytes += len(key) + len(value)
	if s.bytes < s.threshold {
		return nil
	}
	return s.flushAsync()
}

// vertexSink adapts the sink to internal/nimbus.Builder: the RootedVertexID
// becomes the AriVtx row key. Not safe for concurrent use (one scratch key).
func (s *batchSink) vertexSink() nimbusinternal.Sink {
	var keyBuf [20]byte
	return func(rvid nimbusinternal.RootedVertexID, rec []byte) error {
		return s.put(nimbusinternal.AppendKey(keyBuf[:0], rvid), rec)
	}
}

func (s *batchSink) flushAsync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.DisableWAL(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("nimbus: flush batch (cf=%d): %w", s.cfIdx, err)
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
		return fmt.Errorf("nimbus: sync-flush batch (cf=%d): %w", s.cfIdx, err)
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

// preStorage is a spec entity's storage trie as built by Phase 0.
type preStorage struct {
	root    common.Hash
	stoID   nimbusinternal.VertexID // 0 when the entity had no non-zero slot
	stoHint byte
}

// accountWorkItem goes from Stage A (reader) to Stage B (workers). Exactly
// one of pre / stoID is set for accounts with storage: pre for Phase-0
// entities, stoID (allocated by Stage A in sorted order) for the rest.
type accountWorkItem struct {
	seq      uint64
	addrHash common.Hash
	ent      entity
	pre      *preStorage
	stoID    nimbusinternal.VertexID
}

type accountStatDelta struct {
	StorageSlotsCreated uint64
	StorageBytes        uint64
	AccountBytes        uint64
	CodeBytes           uint64
	IsContract          bool
}

// accountResult is what Stage B hands Stage C: the storage rows are already
// written through the worker's own sink, so only scalars travel.
type accountResult struct {
	seq         uint64
	addrHash    common.Hash
	nonce       uint64
	balance     *uint256.Int
	storageRoot common.Hash
	stoID       nimbusinternal.VertexID
	stoHint     byte
	codeHash    common.Hash
	code        []byte
	accountRLP  []byte
	stats       accountStatDelta
	buildErr    error
}

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

func newResultHeap() *resultHeap {
	h := &resultHeap{}
	heap.Init(h)
	return h
}

// ---------------------------------------------------------------------------
// Storage trie build shared by Stage B workers
// ---------------------------------------------------------------------------

type storageSlotKV struct {
	slotHash common.Hash
	value    common.Hash
}

// collectNonZeroSlots returns ent's non-zero slots sorted by keccak(key), or
// nil when there are none.
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
		kvs = append(kvs, storageSlotKV{slotHash: crypto.Keccak256Hash(s.Key[:]), value: s.Value})
	}
	if len(kvs) == 0 {
		return nil
	}
	slices.SortFunc(kvs, func(a, b storageSlotKV) int { return bytes.Compare(a.slotHash[:], b.slotHash[:]) })
	return kvs
}

// hasNonZeroSlot reports whether ent will produce a storage trie.
func hasNonZeroSlot(ent entity) bool {
	var zero common.Hash
	for _, s := range ent.slots {
		if s.Value != zero {
			return true
		}
	}
	return false
}

// buildStorageTrieInline streams ent's storage trie, rooted at stoID, into
// sink and returns the root, the stoHint and the slot stats. Deep branches
// take 16-ID blocks from alloc.
func buildStorageTrieInline(
	stoID nimbusinternal.VertexID,
	ent entity,
	alloc *nimbusinternal.VidAllocator,
	sink nimbusinternal.Sink,
) (root common.Hash, stoHint byte, slots, slotBytes uint64, err error) {
	kvs := collectNonZeroSlots(ent)
	if kvs == nil {
		return nimbusinternal.EmptyRootHash, 0, 0, 0, nil
	}
	sb := nimbusinternal.NewBuilder(stoID, alloc, sink)
	var nib [64]byte
	var payload [40]byte
	for _, e := range kvs {
		valueRLP := ethrex.EncodeStorageValueBytes32(e.value)
		if addErr := sb.AddLeaf(
			ethrex.AppendNibbles(nib[:0], e.slotHash[:]),
			valueRLP,
			nimbusinternal.AppendStoLeafPayload(payload[:0], e.value),
		); addErr != nil {
			return nimbusinternal.EmptyRootHash, 0, 0, 0, fmt.Errorf("nimbus: storage leaf: %w", addErr)
		}
		slots++
		slotBytes += uint64(len(valueRLP))
	}
	root, err = sb.Root()
	if err != nil {
		return nimbusinternal.EmptyRootHash, 0, 0, 0, fmt.Errorf("nimbus: storage root: %w", err)
	}
	return root, sb.StoHint(), slots, slotBytes, nil
}

// ---------------------------------------------------------------------------
// Entity blob codec — the ethrex writer's shape, self-contained.
// ---------------------------------------------------------------------------

type entityKind byte

const (
	entityEOA      entityKind = 1
	entityContract entityKind = 2
)

// entitySlot aliases entitygen.StorageSlot so autofill storage passes through
// without conversion (pointer-free, one GC-opaque backing array per entity).
type entitySlot = entitygen.StorageSlot

type entity struct {
	kind    entityKind
	nonce   uint64
	balance *uint256.Int
	code    []byte
	slots   []entitySlot // raw (key, value); zero values filtered downstream
}

// encodeEntity serialises an entity to the streamsort blob:
//
//	EOA:      [0x01] [nonce u64 BE] [balance_len u8] [balance bytes]
//	contract: [0x02] [nonce u64 BE] [balance_len u8] [balance bytes]
//	          [code_len u32 BE] [code] [slot_count u32 BE] [slot_count × (32B key, 32B value)]
func encodeEntity(nonce uint64, balance *uint256.Int, code []byte, slots []entitySlot) []byte {
	balBytes := balance.Bytes()
	if len(code) == 0 && len(slots) == 0 {
		out := make([]byte, 1+8+1+len(balBytes))
		out[0] = byte(entityEOA)
		binary.BigEndian.PutUint64(out[1:9], nonce)
		out[9] = byte(len(balBytes))
		copy(out[10:], balBytes)
		return out
	}
	out := make([]byte, 0, 1+8+1+len(balBytes)+4+len(code)+4+len(slots)*64)
	out = append(out, byte(entityContract))
	out = binary.BigEndian.AppendUint64(out, nonce)
	out = append(out, byte(len(balBytes)))
	out = append(out, balBytes...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(code)))
	out = append(out, code...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(slots)))
	for _, s := range slots {
		out = append(out, s.Key[:]...)
		out = append(out, s.Value[:]...)
	}
	return out
}

// decodeEntity reverses encodeEntity, returning errors rather than panicking
// because it runs on the Stage A reader goroutine.
func decodeEntity(blob []byte) (entity, error) {
	need := func(pos, n int) error {
		if pos+n > len(blob) {
			return fmt.Errorf("nimbus: truncated entity blob: need %d bytes at offset %d, have %d", n, pos, len(blob))
		}
		return nil
	}
	if err := need(0, 1); err != nil {
		return entity{}, err
	}
	e := entity{kind: entityKind(blob[0])}
	switch e.kind {
	case entityEOA, entityContract:
	default:
		return entity{}, fmt.Errorf("nimbus: unknown entity kind byte %d", blob[0])
	}
	pos := 1
	if err := need(pos, 8); err != nil {
		return entity{}, err
	}
	e.nonce = binary.BigEndian.Uint64(blob[pos : pos+8])
	pos += 8
	if err := need(pos, 1); err != nil {
		return entity{}, err
	}
	balLen := int(blob[pos])
	pos++
	if err := need(pos, balLen); err != nil {
		return entity{}, err
	}
	e.balance = new(uint256.Int).SetBytes(blob[pos : pos+balLen])
	pos += balLen
	if e.kind == entityContract {
		if err := need(pos, 4); err != nil {
			return entity{}, err
		}
		codeLen := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
		pos += 4
		if err := need(pos, codeLen); err != nil {
			return entity{}, err
		}
		e.code = append([]byte(nil), blob[pos:pos+codeLen]...)
		pos += codeLen
		if err := need(pos, 4); err != nil {
			return entity{}, err
		}
		slotCount := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
		pos += 4
		if err := need(pos, slotCount*64); err != nil {
			return entity{}, err
		}
		e.slots = make([]entitySlot, slotCount)
		for i := range e.slots {
			copy(e.slots[i].Key[:], blob[pos:pos+32])
			copy(e.slots[i].Value[:], blob[pos+32:pos+64])
			pos += 64
		}
	}
	return e, nil
}
