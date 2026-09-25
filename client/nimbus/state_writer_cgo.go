//go:build cgo_nimbus

package nimbus

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/ethrex"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// defaultMaxPhase2Workers caps the Stage-B pool at min(NumCPU, 16); Stage C
// (account trie, sequential) and RocksDB's background jobs bound the useful
// parallelism. STATE_ACTOR_NIMBUS_WORKERS overrides.
const defaultMaxPhase2Workers = 16

func phase2Workers() int {
	if v := os.Getenv("STATE_ACTOR_NIMBUS_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("nimbus: ignoring invalid STATE_ACTOR_NIMBUS_WORKERS=%q", v)
	}
	n := runtime.NumCPU()
	if n > defaultMaxPhase2Workers {
		n = defaultMaxPhase2Workers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// writeState builds the whole state from cfg and writes it to db, returning
// the state root. It is the ethrex writer's pipeline with nimbus rows:
//
//  0. Phase 0: spec-entity storage tries stream into AriVtx (per-worker
//     sinks) under stoIDs pre-allocated in addrHash order.
//  1. Phase 1: every entity (autofill EOAs, genesis allocs, autofill
//     contracts — the draw order is the cross-client invariance contract) is
//     spilled into a streamsort keyed by addrHash.
//  2. Phase 2: Stage A reads the sorted stream, stamps seq and allocates a
//     stoID for each remaining entity with a non-zero slot; Stage B workers
//     build those storage tries into per-worker sinks; Stage C applies results
//     in seq order — writes code, then feeds the account leaf (AccLeaf payload
//     with stoID/stoHint) to the account-trie Builder on the shared sink.
//
// After the pipeline the admin record (vTop = allocator high-water mark,
// serial 0) is written with sync. The bulk rows before it skipped the WAL and
// are only made durable by writeGenesisBlock's flush, ahead of the
// canonical-head boot gate.
func writeState(
	ctx context.Context,
	cfg generator.Config,
	db *nimbusDB,
	alloc *nimbusinternal.VidAllocator,
	vertexSink *batchSink,
	codeSink *batchSink,
) (common.Hash, *generator.Stats, error) {
	stats := &generator.Stats{}

	// seenCodeHash deduplicates KvtGen code rows. Owned by Stage C.
	seenCodeHash := make(map[common.Hash]struct{})
	writeCode := func(codeHash common.Hash, code []byte) error {
		if len(code) == 0 {
			return nil // an empty Kvt value is a delete; EmptyCodeHash is implicit
		}
		if _, seen := seenCodeHash[codeHash]; seen {
			return nil
		}
		seenCodeHash[codeHash] = struct{}{}
		if err := codeSink.put(nimbusinternal.KeyCode(codeHash), code); err != nil {
			return fmt.Errorf("nimbus: put code: %w", err)
		}
		return nil
	}

	preStorageByHash := make(map[common.Hash]preStorage)
	if err := runPhase0Storage(ctx, cfg, db, alloc, preStorageByHash, stats); err != nil {
		return common.Hash{}, nil, err
	}

	// Phase 1: spill under the datadir (the one disk sized for the job; see
	// the ethrex writer for the tmpfs hazard).
	sorter, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("nimbus: streamsort.New: %w", err)
	}
	defer sorter.Close()

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))
	plan := cfg.AutoFill
	if plan != nil {
		cfg.Progress.Stage("nimbus: phase 1/2 — generating accounts")
		err := runPhase1Pipeline(ctx, cfg, sorter, plan.NumEOAs, "EOAs", func() phase1Draw {
			acc := plan.DrawEOA(rng)
			return phase1Draw{addrHash: acc.AddrHash, nonce: acc.StateAccount.Nonce, balance: acc.StateAccount.Balance, code: acc.Code}
		})
		if err != nil {
			return common.Hash{}, nil, err
		}
	}

	seenAlloc := make(map[common.Address]struct{}, len(cfg.GenesisAccounts))
	for addr, acc := range cfg.GenesisAccounts {
		if ctx.Err() != nil {
			return common.Hash{}, nil, ctx.Err()
		}
		if _, dup := seenAlloc[addr]; dup {
			continue
		}
		seenAlloc[addr] = struct{}{}
		addrHash := crypto.Keccak256Hash(addr[:])
		balance := acc.Balance
		if balance == nil {
			balance = uint256.NewInt(0)
		}
		code := cfg.GenesisCode[addr]
		var slots []entitySlot
		if storage := cfg.GenesisStorage[addr]; len(storage) > 0 {
			slots = make([]entitySlot, 0, len(storage))
			for k, v := range storage {
				slots = append(slots, entitySlot{Key: k, Value: v})
			}
			slices.SortFunc(slots, func(a, b entitySlot) int { return bytes.Compare(a.Key[:], b.Key[:]) })
		}
		if err := sorter.Put(addrHash[:], encodeEntity(acc.Nonce, balance, code, slots)); err != nil {
			return common.Hash{}, nil, err
		}
	}

	if plan != nil {
		cfg.Progress.Stage("nimbus: phase 1/2 — generating contracts")
		err := runPhase1Pipeline(ctx, cfg, sorter, plan.NumContracts, "contracts", func() phase1Draw {
			contract := plan.DrawContract(rng)
			return phase1Draw{addrHash: contract.AddrHash, nonce: contract.StateAccount.Nonce, balance: contract.StateAccount.Balance, code: contract.Code, slots: contract.Storage}
		})
		if err != nil {
			return common.Hash{}, nil, err
		}
	}

	// Phase 2.
	var entitiesQueued int64
	if plan != nil {
		entitiesQueued += int64(plan.NumEOAs + plan.NumContracts)
	}
	entitiesQueued += int64(len(seenAlloc))
	cfg.Progress.Stage("nimbus: phase 2/2 — building state trie")

	accountBuilder := nimbusinternal.NewBuilder(nimbusinternal.StateRootVID, alloc, vertexSink.vertexSink())
	numWorkers := phase2Workers()
	workCh := make(chan *accountWorkItem, 2*numWorkers)
	resultCh := make(chan *accountResult, 2*numWorkers)

	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()
	var firstErr error
	var errMu sync.Mutex
	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancelPipeline()
		}
		errMu.Unlock()
	}

	// Stage A.
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		defer close(workCh)
		var seq uint64
		iterErr := sorter.Iterate(func(key, value []byte) error {
			if pipelineCtx.Err() != nil {
				return pipelineCtx.Err()
			}
			var addrHash common.Hash
			copy(addrHash[:], key)
			ent, decErr := decodeEntity(value)
			if decErr != nil {
				return decErr
			}
			item := &accountWorkItem{seq: seq, addrHash: addrHash, ent: ent}
			seq++
			if p, ok := preStorageByHash[addrHash]; ok {
				pre := p
				item.pre = &pre
			} else if hasNonZeroSlot(ent) {
				item.stoID = alloc.Alloc(1)
			}
			select {
			case workCh <- item:
				return nil
			case <-pipelineCtx.Done():
				return pipelineCtx.Err()
			}
		})
		if iterErr != nil {
			setErr(iterErr)
		}
	}()

	// Stage B.
	var workerWg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			wSink := newBatchSinkWithThreshold(db, cfIdxAriVtx, workerFlushThresholdBytes)
			defer func() {
				if err := wSink.Close(); err != nil {
					setErr(fmt.Errorf("nimbus: worker sink close: %w", err))
				}
			}()
			vsink := wSink.vertexSink()
			for item := range workCh {
				if pipelineCtx.Err() != nil {
					continue
				}
				res := computeAccountResult(item, alloc, vsink)
				select {
				case resultCh <- res:
				case <-pipelineCtx.Done():
					continue
				}
			}
		}()
	}
	go func() {
		workerWg.Wait()
		close(resultCh)
	}()

	// Stage C.
	reorder := newResultHeap()
	var nextSeq uint64
	var writerErr error
	var nibScratch [64]byte
	var payloadScratch [96]byte

	applyResult := func(res *accountResult) error {
		if res.buildErr != nil {
			return res.buildErr
		}
		if err := writeCode(res.codeHash, res.code); err != nil {
			return err
		}
		payload := nimbusinternal.AppendAccLeafPayload(payloadScratch[:0], res.nonce, res.balance, res.stoID, res.stoHint, res.codeHash)
		if err := accountBuilder.AddLeaf(ethrex.AppendNibbles(nibScratch[:0], res.addrHash[:]), res.accountRLP, payload); err != nil {
			return fmt.Errorf("nimbus: account leaf: %w", err)
		}
		stats.StorageSlotsCreated += int(res.stats.StorageSlotsCreated)
		stats.StorageBytes += res.stats.StorageBytes
		stats.AccountBytes += res.stats.AccountBytes
		stats.CodeBytes += res.stats.CodeBytes
		if res.stats.IsContract {
			stats.ContractsCreated++
		} else {
			stats.AccountsCreated++
		}
		return nil
	}

	for res := range resultCh {
		if writerErr != nil {
			continue
		}
		heap.Push(reorder, res)
		for reorder.Len() > 0 && (*reorder)[0].seq == nextSeq {
			next := heap.Pop(reorder).(*accountResult)
			if err := applyResult(next); err != nil {
				writerErr = err
				setErr(err)
				break
			}
			nextSeq++
			cfg.Progress.Tick(int64(nextSeq), entitiesQueued, "accounts")
		}
	}
	readerWg.Wait()

	errMu.Lock()
	pipelineErr := firstErr
	errMu.Unlock()
	if pipelineErr != nil {
		return common.Hash{}, nil, pipelineErr
	}
	if writerErr != nil {
		return common.Hash{}, nil, writerErr
	}
	if accountBuilder.LeafCount() == 0 {
		return common.Hash{}, nil, errors.New("nimbus: no accounts to write (an empty account trie has no root vertex)")
	}

	stateRoot, err := accountBuilder.Root()
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("nimbus: account trie root: %w", err)
	}
	if err := vertexSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := codeSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	// Admin record: the dynamic-ID high-water mark. Written after every
	// allocation (Phase 0, Stage A/B/C) has finished.
	admin := nimbusinternal.EncodeSavedState(nimbusinternal.SavedState{VTop: alloc.VTop(), Serial: 0})
	if err := db.putSync(cfIdxAriVtx, nimbusinternal.AdminKey, admin); err != nil {
		return common.Hash{}, nil, fmt.Errorf("nimbus: put admin record: %w", err)
	}

	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return stateRoot, stats, nil
}

// computeAccountResult is the Stage-B per-account computation: the storage
// trie (if any) streams into the worker's sink; the result carries scalars.
func computeAccountResult(item *accountWorkItem, alloc *nimbusinternal.VidAllocator, sink nimbusinternal.Sink) *accountResult {
	res := &accountResult{seq: item.seq, addrHash: item.addrHash, nonce: item.ent.nonce, balance: item.ent.balance}
	res.storageRoot = nimbusinternal.EmptyRootHash
	switch {
	case item.pre != nil:
		res.storageRoot, res.stoID, res.stoHint = item.pre.root, item.pre.stoID, item.pre.stoHint
	case item.stoID != 0:
		root, hint, slots, slotBytes, err := buildStorageTrieInline(item.stoID, item.ent, alloc, sink)
		if err != nil {
			res.buildErr = err
			return res
		}
		if slots > 0 {
			res.storageRoot, res.stoID, res.stoHint = root, item.stoID, hint
		}
		res.stats.StorageSlotsCreated = slots
		res.stats.StorageBytes = slotBytes
	}
	codeHash := nimbusinternal.EmptyCodeHash
	if len(item.ent.code) > 0 {
		codeHash = crypto.Keccak256Hash(item.ent.code)
		res.stats.CodeBytes = uint64(len(item.ent.code))
	}
	res.codeHash = codeHash
	res.code = item.ent.code
	res.accountRLP = ethrex.EncodeAccountState(item.ent.nonce, item.ent.balance, res.storageRoot, codeHash)
	res.stats.AccountBytes = uint64(len(res.accountRLP))
	res.stats.IsContract = len(item.ent.code) != 0 || res.storageRoot != nimbusinternal.EmptyRootHash
	return res
}

// ---------------------------------------------------------------------------
// Phase 1: draw → encode → spill (the ethrex writer's pipeline)
// ---------------------------------------------------------------------------

type phase1Draw struct {
	addrHash common.Hash
	nonce    uint64
	balance  *uint256.Int
	code     []byte
	slots    []entitySlot
}

type phase1Blob struct {
	addrHash common.Hash
	blob     []byte
}

const (
	phase1EncodeWorkers = 8
	phase1BatchSize     = 256
)

// runPhase1Pipeline drives count draws through encode workers into the
// sorter. The DRAW stays on the calling goroutine (the RNG sequence is the
// cross-client invariance contract); sorter.Put runs on exactly one goroutine.
func runPhase1Pipeline(ctx context.Context, cfg generator.Config, sorter *streamsort.Store, count int, tickLabel string, draw func() phase1Draw) error {
	workers := phase1EncodeWorkers
	if n := runtime.NumCPU(); n < workers {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}
	p1Ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var firstErr error
	var errMu sync.Mutex
	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		errMu.Unlock()
	}

	drawCh := make(chan []phase1Draw, 2*workers)
	blobCh := make(chan []phase1Blob, 2*workers)

	var encWg sync.WaitGroup
	for w := 0; w < workers; w++ {
		encWg.Add(1)
		go func() {
			defer encWg.Done()
			for batch := range drawCh {
				if p1Ctx.Err() != nil {
					continue
				}
				out := make([]phase1Blob, len(batch))
				for i, d := range batch {
					out[i] = phase1Blob{addrHash: d.addrHash, blob: encodeEntity(d.nonce, d.balance, d.code, d.slots)}
				}
				select {
				case blobCh <- out:
				case <-p1Ctx.Done():
				}
			}
		}()
	}
	go func() {
		encWg.Wait()
		close(blobCh)
	}()

	var putWg sync.WaitGroup
	putWg.Add(1)
	go func() {
		defer putWg.Done()
		var done int64
		for batch := range blobCh {
			if p1Ctx.Err() != nil {
				continue
			}
			for _, b := range batch {
				if err := sorter.Put(b.addrHash[:], b.blob); err != nil {
					setErr(err)
					break
				}
				done++
				cfg.Progress.Tick(done, int64(count), tickLabel)
			}
		}
	}()

	batch := make([]phase1Draw, 0, phase1BatchSize)
	for i := 0; i < count; i++ {
		if p1Ctx.Err() != nil {
			break
		}
		batch = append(batch, draw())
		if len(batch) == phase1BatchSize {
			select {
			case drawCh <- batch:
			case <-p1Ctx.Done():
			}
			batch = make([]phase1Draw, 0, phase1BatchSize)
		}
	}
	if len(batch) > 0 && p1Ctx.Err() == nil {
		select {
		case drawCh <- batch:
		case <-p1Ctx.Done():
		}
	}
	close(drawCh)
	putWg.Wait()

	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}
