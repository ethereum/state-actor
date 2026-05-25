//go:build cgo_reth

package reth

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	iReth "github.com/nerolation/state-actor/internal/reth"
	"github.com/nerolation/state-actor/internal/streamingtrie"
)

// maxStreamSpecStorageWorkers caps drain-and-compute parallelism; each worker
// owns a streamsort.Store so peak RAM scales linearly.
const maxStreamSpecStorageWorkers = 8

// chunkSlots is the slot-batch size workers emit to the consumer; bounds
// in-flight memory per entity.
const chunkSlots = 1024

// chunkChanCap = 4 lets a worker run ~4 chunks ahead of the consumer to hide
// per-txn commit latency without unbounded RAM growth.
const chunkChanCap = 4

// slotPrepared holds the per-slot byte buffers + raw key a single slot
// contributes. Produced on the worker goroutine, consumed on the main
// goroutine inside Mdbx.Update — no encoding work in the hot write path.
// rawKey lets the consumer route an archive-mode StoragesHistory put
// into the RocksDB CF (envs.HistorySink builds the StorageShardedKey
// internally).
type slotPrepared struct {
	rawKey      common.Hash // slot key (untouched); StoragesHistory sink arg
	hashedEntry []byte      // HashedStorages value (encoded keyHash || value)
	changeEntry []byte      // StorageChangeSets value (encoded rawKey || 0); nil in full mode
}

// preparedEntity carries everything the consumer needs to commit one
// entity's storage. The worker fills chunkCh in keccak-ascending order,
// then sends the computed root via rootCh and closes chunkCh. errCh
// carries any IterateRoot failure surfacing on the worker side. trieRows
// are pre-encoded StoragesTrie DupSort values (each entry = packed
// SubKey(33 bytes) || BranchNodeCompact bytes); the consumer writes them
// under the DupSort main key ent.addrHash after the per-slot rows land.
// Emitted in path-lex order so cursor.AppendDup takes the fast path.
type preparedEntity struct {
	idx      int
	addr     common.Address
	addrHash common.Hash
	blockKey []byte // BlockNumberAddress-encoded
	chunkCh  chan []slotPrepared
	rootCh   chan common.Hash
	errCh    chan error
	trieRows [][]byte
}

// streamSpecStorage writes each PreAlloc entity's Storage into reth's
// four storage tables, computes the storage MPT root, and splices it
// into cfg.GenesisAccounts[addr].Root for the alloc handler.
//
// Architecture: N parallel workers each (a) drain their entity's iter
// into a per-call streamsort.Store, (b) walk the sorted store driving
// the HashBuilder and pre-encoding all 4 per-slot byte buffers into
// chunks. The single consumer goroutine opens one Mdbx.Update per
// entity and does just the 4 txn.Put calls per slot — the encoding,
// keccak, and HashBuilder work happens on the workers.
//
// Per-entity Mdbx.Update — not one global txn — because a single
// global txn wrapping ~1.5B Puts degrades MDBX's dirty-page tree non-
// linearly. MDBX enforces single-writer-per-env so the commits run
// serially anyway; the win is offloading every cycle of CPU work to
// the workers so the consumer is pure I/O.
func streamSpecStorage(ctx context.Context, envs *Envs, cfg *generator.Config, stats *generator.Stats) error {
	if len(cfg.PreAlloc) == 0 {
		return nil
	}
	indices := make([]int, 0, len(cfg.PreAlloc))
	for i := range cfg.PreAlloc {
		if cfg.PreAlloc[i].Storage != nil {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return nil
	}

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > maxStreamSpecStorageWorkers {
		workers = maxStreamSpecStorageWorkers
	}
	if workers > len(indices) {
		workers = len(indices)
	}

	drainCtx, cancelDrain := context.WithCancelCause(ctx)
	defer cancelDrain(nil)

	entityCh := make(chan int, workers*2)
	// preparedCh buffer of workers*2 absorbs small latency spikes; the
	// consumer applies back-pressure via the per-entity chunkCh inside
	// each preparedEntity.
	preparedCh := make(chan *preparedEntity, workers*2)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range entityCh {
				if drainCtx.Err() != nil {
					return
				}
				if err := drainAndEncodeEntity(drainCtx, cfg, i, preparedCh); err != nil {
					cancelDrain(err)
					return
				}
			}
		}()
	}

	go func() {
		defer close(entityCh)
		for _, i := range indices {
			select {
			case entityCh <- i:
			case <-drainCtx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(preparedCh)
	}()

	var totalStorageBytes uint64
	var writeErr error
	for ent := range preparedCh {
		if writeErr != nil {
			// Drain remaining chunks so the worker doesn't block on send.
			for range ent.chunkCh {
			}
			continue
		}
		bytesWritten, err := consumeEntity(ctx, envs, cfg, ent)
		if err != nil {
			writeErr = err
			cancelDrain(err)
			continue
		}
		if acc, ok := cfg.GenesisAccounts[ent.addr]; ok && acc != nil {
			// Wait for the worker's computed root. By the time chunkCh
			// closed (consumeEntity returns), exactly one of {rootCh,
			// errCh} has a value.
			select {
			case root := <-ent.rootCh:
				acc.Root = root
			case err := <-ent.errCh:
				writeErr = err
				cancelDrain(err)
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		totalStorageBytes += bytesWritten
	}

	if writeErr != nil {
		return writeErr
	}
	if cause := context.Cause(drainCtx); cause != nil {
		return cause
	}

	if stats != nil {
		stats.StorageBytes += totalStorageBytes
	}
	return nil
}

// drainAndEncodeEntity runs on a worker goroutine. It drains the
// entity's storage iter into a streamsort, then walks the sorted store
// driving the HashBuilder AND pre-encoding the per-slot byte buffers
// into chunks sent to the consumer via ent.chunkCh.
func drainAndEncodeEntity(
	drainCtx context.Context,
	cfg *generator.Config,
	idx int,
	preparedCh chan<- *preparedEntity,
) error {
	pe := &cfg.PreAlloc[idx]
	addr := pe.Address
	addrHash := crypto.Keccak256Hash(addr[:])

	d, err := streamingtrie.Drain(cfg.DBPath, pe.Storage)
	if err != nil {
		return fmt.Errorf("reth: drain spec storage[%d] %s: %w", idx, addr.Hex(), err)
	}

	blockKey := iReth.BlockNumberAddress{BlockNumber: 0, Address: addr}
	var blockKeyBuf bytes.Buffer
	blockKey.EncodeKey(&blockKeyBuf)

	ent := &preparedEntity{
		idx:      idx,
		addr:     addr,
		addrHash: addrHash,
		blockKey: blockKeyBuf.Bytes(),
		chunkCh:  make(chan []slotPrepared, chunkChanCap),
		rootCh:   make(chan common.Hash, 1),
		errCh:    make(chan error, 1),
	}

	// Hand the preparedEntity to the consumer BEFORE starting iteration
	// so the consumer can begin draining chunks as soon as the first one
	// is ready. The consumer is responsible for closing the Drained
	// indirectly by waiting for chunkCh to close.
	select {
	case preparedCh <- ent:
	case <-drainCtx.Done():
		d.Close()
		return drainCtx.Err()
	}

	archive := cfg.Archive

	// Per-entity trie emissions: HashBuilder emits in path-lex order as
	// it walks the sorted slots. Worker pre-encodes each emission as a
	// packed StorageTrieEntry value (33-byte SubKey || BNC; v2 format)
	// and accumulates into trieRows. The consumer drains trieRows into
	// StoragesTrie within the same per-entity Mdbx.Update txn AFTER the
	// slot rows land — keeping the cursor.AppendDup fast path intact.
	// Memory per entity: O(trie nodes) ≈ small multiple of slot count.
	trieRows := make([][]byte, 0, 64)
	emit := func(path iReth.StoredNibbles, node iReth.BranchNodeCompact) error {
		if path.Length == 0 {
			// Skip root branch — see TestNoRootCacheRows.
			return nil
		}
		var valBuf bytes.Buffer
		entry := iReth.StorageTrieEntry{SubKey: path, Node: node}
		entry.EncodePackedCompact(&valBuf)
		trieRows = append(trieRows, valBuf.Bytes())
		return nil
	}
	hb := newRethStorageHashBuilderWithEmit(emit)
	pending := make([]slotPrepared, 0, chunkSlots)

	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		select {
		case ent.chunkCh <- pending:
			pending = make([]slotPrepared, 0, chunkSlots)
			return nil
		case <-drainCtx.Done():
			return drainCtx.Err()
		}
	}

	sink := func(keyHash, rawKey, value common.Hash) error {
		slotValueU256 := uint256.NewInt(0).SetBytes(value[:])

		prep := slotPrepared{rawKey: rawKey}

		hashedEntry := iReth.StorageEntry{Key: keyHash, Value: slotValueU256}
		var hashedBuf bytes.Buffer
		hashedEntry.EncodeCompact(&hashedBuf)
		prep.hashedEntry = hashedBuf.Bytes()

		if archive {
			changeEntry := iReth.StorageEntry{Key: rawKey, Value: uint256.NewInt(0)}
			var changeBuf bytes.Buffer
			changeEntry.EncodeCompact(&changeBuf)
			prep.changeEntry = changeBuf.Bytes()
		}

		pending = append(pending, prep)
		if len(pending) >= chunkSlots {
			return flushPending()
		}
		return nil
	}

	root, err := d.IterateRoot(hb, sink)
	d.Close()
	if err != nil {
		ent.errCh <- fmt.Errorf("reth: iterate spec storage[%d] %s: %w", idx, addr.Hex(), err)
		close(ent.chunkCh)
		return nil
	}

	// Flush trailing chunk (if any) before closing the channel.
	if err := flushPending(); err != nil {
		ent.errCh <- err
		close(ent.chunkCh)
		return nil
	}
	// Stash trie emissions on the preparedEntity BEFORE closing chunkCh
	// so the consumer sees them when the for-range loop exits.
	ent.trieRows = trieRows
	close(ent.chunkCh)
	ent.rootCh <- root
	return nil
}

// consumeEntity runs on the main goroutine. Opens one Mdbx.Update per
// entity and drains the worker's chunks into 4 txn.Put calls per slot.
// All bytes are pre-encoded by the worker; this function does pure I/O.
func consumeEntity(
	ctx context.Context,
	envs *Envs,
	cfg *generator.Config,
	ent *preparedEntity,
) (uint64, error) {
	var localBytes uint64
	err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for chunk := range ent.chunkCh {
			for _, prep := range chunk {
				if err := txn.Put(envs.MdbxDBIs["HashedStorages"], ent.addrHash[:], prep.hashedEntry, 0); err != nil {
					return fmt.Errorf("HashedStorages %s: %w", ent.addrHash.Hex(), err)
				}
				if prep.changeEntry != nil {
					// Archive mode: write StorageChangeSets (MDBX) + route
					// StoragesHistory to RocksDB CF (v2).
					if err := txn.Put(envs.MdbxDBIs["StorageChangeSets"], ent.blockKey, prep.changeEntry, 0); err != nil {
						return fmt.Errorf("StorageChangeSets %s: %w", ent.addr.Hex(), err)
					}
					if err := envs.HistorySink().PutStorageHistory(ent.addr, prep.rawKey, 0); err != nil {
						return fmt.Errorf("StoragesHistory %s: %w", ent.addr.Hex(), err)
					}
				}
				// Stats: bank HashedStorages entry bytes.
				localBytes += uint64(len(prep.hashedEntry))
			}
		}
		// Drain pre-encoded trie rows into StoragesTrie keyed by
		// keccak(address). Plain Put (not AppendDup): entities are
		// processed in PreAlloc index order (not addrHash-sorted), so
		// AppendDup would trip MDBX_EKEYMISMATCH on cross-entity
		// boundaries. Sub-key ordering is preserved by MDBX's per-key
		// dup-sort regardless.
		if len(ent.trieRows) > 0 {
			for _, row := range ent.trieRows {
				if err := txn.Put(envs.MdbxDBIs["StoragesTrie"], ent.addrHash[:], row, 0); err != nil {
					return fmt.Errorf("StoragesTrie %s: %w", ent.addr.Hex(), err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return localBytes, nil
}

// rethStorageHashBuilder adapts iReth.HashBuilder to streamingtrie.HashBuilder.
type rethStorageHashBuilder struct {
	hb *iReth.HashBuilder
}

func newRethStorageHashBuilder() *rethStorageHashBuilder {
	return &rethStorageHashBuilder{
		hb: iReth.NewHashBuilder(func(_ iReth.StoredNibbles, _ iReth.BranchNodeCompact) error {
			return nil
		}),
	}
}

// newRethStorageHashBuilderWithEmit returns a HashBuilder configured for
// full-emissions mode: every branch with RLP ≥ 32 bytes triggers emit.
// state-actor's spec-storage workers use this so each per-entity storage
// trie populates StoragesTrie alongside the per-slot data rows.
func newRethStorageHashBuilderWithEmit(emit iReth.NodeEmitter) *rethStorageHashBuilder {
	return &rethStorageHashBuilder{
		hb: iReth.NewHashBuilderFullEmissions(emit),
	}
}

func (r *rethStorageHashBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return r.hb.AddLeaf(addrHashToNibbles(keyHash[:]), valueRLP)
}

func (r *rethStorageHashBuilder) Root() (common.Hash, error) {
	return r.hb.Root(), nil
}
