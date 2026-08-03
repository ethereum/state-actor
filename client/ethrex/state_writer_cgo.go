//go:build cgo_ethrex

package ethrex

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
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
	"github.com/ethereum/state-actor/internal/entitygen"
	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// defaultMaxPhase2Workers caps the Stage-B pool at min(NumCPU, 16).
//
// The old justification for 8 — each in-flight account held up to ~64 MiB of
// buffered storage rows — died with the buffering: workers now stream rows
// straight into their own batchSink pair, so a result is scalars and the
// per-worker memory term is the sink batches (2 × ~2×workerFlushThresholdBytes
// ≈ 64 MiB C heap per worker, ≤1 GiB at 16). 16 rather than NumCPU because
// Stage C (account trie + code writes, strictly sequential) and RocksDB's
// 8 background jobs bound the useful parallelism; measurement showed 96
// workers only ~17% faster than 8 under the OLD buffered design — the ladder
// re-measures under this one. Overridable for experiments, erigon-style.
const defaultMaxPhase2Workers = 16

// phase2Workers resolves the Stage-B pool size, honoring the
// STATE_ACTOR_ETHREX_WORKERS env override (same pattern as erigon's
// STATE_ACTOR_ERIGON_WORKERS: "to exploit many-core hosts").
func phase2Workers() int {
	if v := os.Getenv("STATE_ACTOR_ETHREX_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("ethrex: ignoring invalid STATE_ACTOR_ETHREX_WORKERS=%q", v)
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

// writeState builds the full ethrex state from cfg and writes it to db.
// Returns the state root hash and stats.
//
// Pipeline:
//  1. Handle PreAlloc storage: for each PreAlloc entity with storage,
//     build the storage trie in-memory and record its root.
//  2. Queue all entities (AutoFill EOAs, AutoFill contracts, GenesisAccounts)
//     into a streamsort.Store keyed by addrHash.
//  3. Iterate sorted with a 3-stage parallel pipeline:
//     Stage A (reader): pull (addrHash, entity) in addrHash order, stamp seq.
//     Stage B (N workers): compute storage trie in memory for normal accounts.
//     Stage C (writer): apply results in strict seq order — always addrHash-sorted.
//
// RAM bound: internal/ethrex.Builder is streaming — O(keyLen), independent of
// leaf count (see its doc comment). Storage rows stream from Stage-B workers
// straight into per-worker sinks (no per-account buffering), so the Go-heap
// terms that scale with the run are seenCodeHash (one entry per distinct
// REAL contract code) and the scalar in-flight pipeline (~5×numWorkers tiny
// results). Everything else lives off-heap in RocksDB and Pebble; see
// doc.go's "Memory" section for the whole budget.
//
// Flat-KV / snap-sync layout: leaf full-path rows are routed ONLY to the
// flat-KV CFs (never duplicated into the trie-node CFs), modelling a snap-synced
// node; writeState stamps misc_values["last_written"]=0xff so ethrex skips
// regeneration on boot. See the package doc (doc.go, "Flat-KV layer") for the
// full rationale and the ethrex source it mirrors.
func writeState(
	ctx context.Context,
	cfg generator.Config,
	db *ethrexDB,
	accountSink *batchSink,
	accountFkvSink *batchSink,
	codeSink *batchSink,
	codeMetaSink *batchSink,
) (common.Hash, *generator.Stats, error) {
	stats := &generator.Stats{}

	emptyCodeHash := common.HexToHash(ethrexinternal.EmptyCodeHashHex)
	emptyTrieHash := common.HexToHash(ethrexinternal.EmptyTrieHashHex)

	// seenCodeHash deduplicates account_codes + account_code_metadata writes
	// for REAL contract code only (≈ plan.NumContracts entries). Owned
	// exclusively by Stage C.
	//
	// EIP-7702 delegation designators are deliberately NOT deduplicated:
	// 30% of autofill EOAs carry one with a unique random target
	// (internal/autofill/eoa_flavor.go), so tracking them grew this map to
	// ~57 M entries ≈ 2.4-4.9 GiB — the dominant Go-heap term of a large
	// run. A duplicate put of an identical (hash → code) pair is idempotent
	// (same key, same value; RocksDB last-write-wins and compaction
	// collapses it), so dedup buys nothing there but the map itself.
	seenCodeHash := make(map[common.Hash]struct{})

	// writeCode writes code for a given codeHash if not already seen.
	// Always writes even for empty code (ethrex stores the empty-code entry).
	writeCode := func(codeHash common.Hash, code []byte) error {
		isDelegation := len(code) == 23 &&
			code[0] == 0xEF && code[1] == 0x01 && code[2] == 0x00
		if !isDelegation {
			if _, seen := seenCodeHash[codeHash]; seen {
				return nil
			}
			seenCodeHash[codeHash] = struct{}{}
		}
		encoded := ethrexinternal.EncodeCode(code)
		if err := codeSink.put(codeHash[:], encoded); err != nil {
			return fmt.Errorf("ethrex: put account_codes: %w", err)
		}
		meta := ethrexinternal.CodeLengthMetadata(code)
		if err := codeMetaSink.put(codeHash[:], meta[:]); err != nil {
			return fmt.Errorf("ethrex: put account_code_metadata: %w", err)
		}
		return nil
	}

	// Write empty-code entry up-front (every account needs it written once).
	if err := writeCode(emptyCodeHash, nil); err != nil {
		return common.Hash{}, nil, err
	}

	// accountTrieNodeSink applies the snap-sync split: leaf full-path rows go ONLY
	// to account_flatkeyvalue, structural/leaf-node rows to account_trie_nodes.
	// See the package doc (doc.go, "Flat-KV layer") for the rationale.
	accountTrieNodeSink := ethrexinternal.NodeSink(func(path []byte, value []byte) error {
		if isLeafFullPathHelper(path) {
			return accountFkvSink.put(path, value)
		}
		return accountSink.put(path, value)
	})

	// Storage tries apply the same split: Phase 0 in its per-worker sinks, Phase 2
	// inline when replaying buffered rows / building big-account storage.

	// preAllocStorageRoots maps addrHash → computed storage root for PreAlloc
	// entities whose storage was built in-memory.
	preAllocStorageRoots := make(map[common.Hash]common.Hash)

	// Phase 0: drain PreAlloc entity storage tries in parallel. Each worker owns
	// its own per-worker batchSinks (storage_trie_nodes + storage_flatkeyvalue)
	// so concurrent RocksDB writes are safe — each entity's addrHash prefix
	// gives it a disjoint keyspace. Roots are content-addressed so worker
	// completion order does not affect the final state root.
	if err := runPhase0Storage(ctx, cfg, db, preAllocStorageRoots, stats); err != nil {
		return common.Hash{}, nil, err
	}

	// Phase 1: queue all entities into a streamsort keyed by addrHash.
	//
	// Spill under the datadir, not os.TempDir(): the spill is ~0.4× the
	// target size (~130 GB for a 350 GB run), and "" would put it wherever
	// /tmp points — tmpfs (i.e. RAM) on some hosts, container storage under
	// docker/podman — invisible either way. The datadir volume is the one
	// disk a state-actor run is guaranteed to have sized for the job. Same
	// choice as this writer's own Phase 0 (streamingtrie.StorageRoot gets
	// cfg.DBPath), reth, and erigon — erigon's comment documents the tmpfs
	// hazard explicitly. Store.Close removes the spill dir before the DB
	// handle closes, so the shipped datadir stays clean.
	sorter, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: streamsort.New: %w", err)
	}
	defer sorter.Close()

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))

	plan := cfg.AutoFill
	if plan != nil {
		cfg.Progress.Stage("ethrex: phase 1/2 — generating accounts")
		err := runPhase1Pipeline(ctx, cfg, sorter, plan.NumEOAs, "EOAs", func() phase1Draw {
			acc := plan.DrawEOA(rng)
			return phase1Draw{
				addrHash: acc.AddrHash,
				nonce:    acc.StateAccount.Nonce,
				balance:  acc.StateAccount.Balance,
				code:     acc.Code,
			}
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
			// Sort by raw key for deterministic blob bytes (map iteration
			// is random). Downstream order is irrelevant — Stage B re-sorts
			// by keccak — this is spill reproducibility only.
			slices.SortFunc(slots, func(a, b entitySlot) int {
				return bytes.Compare(a.Key[:], b.Key[:])
			})
		}
		blob := encodeEntity(acc.Nonce, balance, code, slots)
		if err := sorter.Put(addrHash[:], blob); err != nil {
			return common.Hash{}, nil, err
		}
	}

	if plan != nil {
		cfg.Progress.Stage("ethrex: phase 1/2 — generating contracts")
		// contract.Storage passes through directly (entitySlot aliases
		// entitygen.StorageSlot) — the previous per-contract map rebuild did
		// ~750 M pointless inserts per 150 GB run on the serial draw loop.
		err := runPhase1Pipeline(ctx, cfg, sorter, plan.NumContracts, "contracts", func() phase1Draw {
			contract := plan.DrawContract(rng)
			return phase1Draw{
				addrHash: contract.AddrHash,
				nonce:    contract.StateAccount.Nonce,
				balance:  contract.StateAccount.Balance,
				code:     contract.Code,
				slots:    contract.Storage,
			}
		})
		if err != nil {
			return common.Hash{}, nil, err
		}
	}

	// Phase 2: 3-stage ordered parallel pipeline.
	//
	// Ordering: the ACCOUNT CFs (account_trie_nodes, account_flatkeyvalue)
	// stay addrHash-ascending — the account-trie Builder requires it, and only
	// Stage C feeds it, in strict seq (= sorter output) order via the reorder
	// heap. The STORAGE CFs are written by Stage-B workers through per-worker
	// sinks in arbitrary cross-account order — exactly how Phase 0 has always
	// written these same two CFs (per-account keyspaces are disjoint;
	// RocksDB serialises concurrent Write internally — besu's writer
	// documents the same). The memtable path is order-agnostic.
	//
	// Stage A (single reader goroutine):
	//   sorter.Iterate yields (addrHash, entity) in addrHash-ascending order.
	//   Each item is stamped with a monotonically increasing seq and sent to
	//   the work channel.
	//
	// Stage B (phase2Workers goroutines):
	//   Each worker owns a (storage_trie_nodes, storage_flatkeyvalue) batchSink
	//   pair and STREAMS the storage trie build directly into it — no
	//   per-account buffering, so account size does not drive memory and the
	//   old bigAccount special case is gone. Emits a scalar-only result.
	//
	// Stage C (the calling goroutine):
	//   A min-heap reorder buffer drains results in strict seq order.
	//   Per account: (1) dedup and write code; (2) add the account leaf to
	//   accountBuilder; (3) accumulate stats.

	// entitiesQueued is the exact count Put into the sorter above (synthetic
	// EOAs/contracts + deduped genesis allocs) — the Phase-2 progress total.
	var entitiesQueued int64
	if plan != nil {
		entitiesQueued += int64(plan.NumEOAs + plan.NumContracts)
	}
	entitiesQueued += int64(len(seenAlloc))
	cfg.Progress.Stage("ethrex: phase 2/2 — building state trie")

	accountBuilder := ethrexinternal.NewBuilder(accountTrieNodeSink)

	numWorkers := phase2Workers()

	// Bounded channels: cap in-flight work at 2*numWorkers so buffered storage
	// row memory is bounded and Stage A cannot race far ahead of Stage C.
	workCh := make(chan *accountWorkItem, 2*numWorkers)
	resultCh := make(chan *accountResult, 2*numWorkers)

	// pipelineCtx is cancelled on any error to abort all goroutines.
	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()

	// firstErr captures the first error from any goroutine (reader or writer).
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

	// Stage A: single reader goroutine — stamps seq and dispatches work items.
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

			item := &accountWorkItem{
				seq:      seq,
				addrHash: addrHash,
				ent:      ent,
			}
			seq++

			if preRoot, ok := preAllocStorageRoots[addrHash]; ok {
				item.hasPreAllocRoot = true
				item.preAllocRoot = preRoot
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

	// Stage B: worker pool — streams each account's storage trie build into a
	// per-worker sink pair (Phase 0's pattern for these same CFs).
	var workerWg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			wTrieSink := newBatchSinkWithThreshold(db, cfIdxStorageTrieNodes, workerFlushThresholdBytes)
			wFkvSink := newBatchSinkWithThreshold(db, cfIdxStorageFlatKeyValue, workerFlushThresholdBytes)
			defer func() {
				if err := wTrieSink.Close(); err != nil {
					setErr(fmt.Errorf("ethrex: worker storage sink close: %w", err))
				}
				if err := wFkvSink.Close(); err != nil {
					setErr(fmt.Errorf("ethrex: worker storage fkv sink close: %w", err))
				}
			}()
			for item := range workCh {
				if pipelineCtx.Err() != nil {
					// Pipeline cancelled; drain workCh to unblock Stage A.
					continue
				}
				res := computeAccountResult(item, emptyCodeHash, emptyTrieHash, wTrieSink, wFkvSink)
				select {
				case resultCh <- res:
				case <-pipelineCtx.Done():
					continue
				}
			}
		}()
	}

	// Close resultCh once all workers finish.
	go func() {
		workerWg.Wait()
		close(resultCh)
	}()

	// Stage C: single writer goroutine — strictly in-seq application.
	// Runs on the calling goroutine after launching Stage A and B goroutines.
	// The heap is bounded by the in-flight window in the steady state and can
	// transiently exceed it under seq skew (moving a result off resultCh frees
	// the slot for more work) — but results are now scalars, so even large
	// skew costs kilobytes, not buffered row sets.
	reorder := newResultHeap()
	var nextSeq uint64
	var writerErr error

	// Reused nibble scratch for the account leaf (Stage C is single-threaded;
	// AddLeaf copies its key — NodeSink borrow contract).
	var acctNibScratch [64]byte

	applyResult := func(res *accountResult) error {
		if res.buildErr != nil {
			return res.buildErr
		}

		// Storage rows were already streamed to RocksDB by the Stage-B worker;
		// only the strictly-ordered account-side work happens here.

		// Write code (dedup via seenCodeHash; Stage C owns this map).
		if len(res.code) > 0 {
			if err := writeCode(res.codeHash, res.code); err != nil {
				return err
			}
		}

		if err := accountBuilder.AddLeaf(
			ethrexinternal.AppendNibbles(acctNibScratch[:0], res.addrHash[:]),
			res.accountRLP,
		); err != nil {
			return fmt.Errorf("ethrex: account leaf: %w", err)
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
			// Pipeline error already set; drain resultCh to let goroutines finish.
			continue
		}
		heap.Push(reorder, res)
		// Drain all consecutively in-seq results.
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

	// Wait for reader to complete (it may have set firstErr).
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

	// Finalize account trie.
	stateRoot, err := accountBuilder.Root()
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: account trie root: %w", err)
	}

	// Flush trie + flat-KV sinks.
	if err := accountSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := accountFkvSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	// Code sinks too: their defer Close() in run_cgo.go drops the flush error, so
	// flush them explicitly here before the completion gate is stamped below.
	if err := codeSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := codeMetaSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}

	// Mark the flat-KV layer fully generated so ethrex's background generator
	// short-circuits on boot instead of clearing + rebuilding the CFs we wrote.
	if err := db.putSync(cfIdxMiscValues, ethrexinternal.MiscValuesLastWrittenKey, ethrexinternal.FKVLastWrittenComplete); err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: put misc_values last_written: %w", err)
	}

	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return stateRoot, stats, nil
}

// phase1Draw carries one drawn entity from the RNG goroutine to the encode
// workers. Every referenced slice is freshly allocated by the draw, so
// hand-off is ownership transfer, not borrowing.
type phase1Draw struct {
	addrHash common.Hash
	nonce    uint64
	balance  *uint256.Int
	code     []byte
	slots    []entitySlot
}

// phase1Blob is one encoded spill entry headed for the single Put goroutine.
type phase1Blob struct {
	addrHash common.Hash
	blob     []byte
}

// phase1EncodeWorkers sizes the encode pool. Encoding is cheap per entity;
// the serial ends (RNG draw, sorter.Put) bound the pipeline, so a small pool
// suffices.
const phase1EncodeWorkers = 8

// runPhase1Pipeline drives count draws through encode workers into the
// sorter. The DRAW stays on the calling goroutine — the RNG sequence is the
// cross-client invariance contract (erigon's writer documents the same
// split: "the RNG draw stays single-threaded on the main goroutine for
// cross-client invariance; only the CPU-bound encode is parallelised").
// sorter.Put runs on exactly one goroutine (streamsort is single-writer);
// Put ORDER is irrelevant — keys are unique addrHashes and streamsort sorts.
func runPhase1Pipeline(
	ctx context.Context,
	cfg generator.Config,
	sorter *streamsort.Store,
	count int,
	tickLabel string,
	draw func() phase1Draw,
) error {
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

	drawCh := make(chan phase1Draw, 4*workers)
	blobCh := make(chan phase1Blob, 4*workers)

	var encWg sync.WaitGroup
	for w := 0; w < workers; w++ {
		encWg.Add(1)
		go func() {
			defer encWg.Done()
			for d := range drawCh {
				if p1Ctx.Err() != nil {
					continue // drain to unblock the draw goroutine
				}
				b := phase1Blob{
					addrHash: d.addrHash,
					blob:     encodeEntity(d.nonce, d.balance, d.code, d.slots),
				}
				select {
				case blobCh <- b:
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
		for b := range blobCh {
			if p1Ctx.Err() != nil {
				continue // drain
			}
			if err := sorter.Put(b.addrHash[:], b.blob); err != nil {
				setErr(err)
				continue
			}
			done++
			cfg.Progress.Tick(done, int64(count), tickLabel)
		}
	}()

	// The draw loop: sequential, on this goroutine, in index order.
	for i := 0; i < count; i++ {
		if p1Ctx.Err() != nil {
			break
		}
		d := draw()
		select {
		case drawCh <- d:
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

// computeAccountResult is the per-account computation run by Stage B workers.
// Storage rows stream directly into the worker's own sink pair (wTrieSink /
// wFkvSink) during the build — the result carries only scalars.
//
// For hasPreAllocRoot items: uses the pre-computed root directly (Phase 0
// already wrote that entity's storage rows).
func computeAccountResult(
	item *accountWorkItem,
	emptyCodeHash, emptyTrieHash common.Hash,
	wTrieSink, wFkvSink *batchSink,
) *accountResult {
	res := &accountResult{
		seq:      item.seq,
		addrHash: item.addrHash,
	}

	storageRoot := emptyTrieHash

	if item.hasPreAllocRoot {
		storageRoot = item.preAllocRoot
	} else if len(item.ent.slots) > 0 {
		root, slotCount, slotBytes, err := buildStorageTrieInline(item.addrHash, item.ent, emptyTrieHash, wTrieSink, wFkvSink)
		if err != nil {
			res.buildErr = err
			return res
		}
		storageRoot = root
		res.stats.StorageSlotsCreated = slotCount
		res.stats.StorageBytes = slotBytes
	}
	res.storageRoot = storageRoot

	// Code hash (pure; always computed here, even for big accounts, since it
	// requires no storage root knowledge).
	codeHash := emptyCodeHash
	if len(item.ent.code) > 0 {
		codeHash = crypto.Keccak256Hash(item.ent.code)
		res.stats.CodeBytes = uint64(len(item.ent.code))
	}
	res.codeHash = codeHash
	res.code = item.ent.code

	// storageRoot is final here for every account, so accountRLP always
	// pre-computes in the worker.
	accountRLP := ethrexinternal.EncodeAccountState(item.ent.nonce, item.ent.balance, storageRoot, codeHash)
	res.accountRLP = accountRLP
	res.stats.AccountBytes = uint64(len(accountRLP))
	res.stats.IsContract = len(item.ent.code) != 0 || storageRoot != emptyTrieHash

	return res
}

// ---------------------------------------------------------------------------
// Entity blob codec — identical shape to besu's but self-contained.
// ---------------------------------------------------------------------------

type entityKind byte

const (
	entityEOA      entityKind = 1
	entityContract entityKind = 2
)

// entitySlot is one raw (key, value) storage pair. Aliased to
// entitygen.StorageSlot so autofill contract storage passes through with no
// conversion. Deliberately pointer-free (geth's entityBlobSlot precedent,
// client/geth/entity_blob.go): a []entitySlot backing array is one GC-opaque
// block, where the previous map[common.Hash]*uint256.Int made every one of
// ~750 M slots an independently traced heap object and every entity a
// build-map → random-order-encode → rebuild-map round trip.
type entitySlot = entitygen.StorageSlot

type entity struct {
	kind    entityKind
	nonce   uint64
	balance *uint256.Int
	code    []byte
	// slots holds raw (key, value) pairs in blob order. Zero values are
	// filtered by collectNonZeroSlots; order is irrelevant downstream
	// (Stage B re-sorts by keccak(slotKey)). Keys are unique by
	// construction: spec storage comes from a map, autofill keys are
	// 32-byte RNG draws.
	slots []entitySlot
}

// encodeEntity serialises an entity to the streamsort blob.
//
// Format (EOA):
//
//	[0x01] [nonce u64 BE] [balance_len u8] [balance bytes]
//
// Format (contract):
//
//	[0x02] [nonce u64 BE] [balance_len u8] [balance bytes]
//	[code_len u32 BE] [code bytes]
//	[slot_count u32 BE] [slot_count × (32B key, 32B value)]
func encodeEntity(nonce uint64, balance *uint256.Int, code []byte, slots []entitySlot) []byte {
	if len(code) == 0 && len(slots) == 0 {
		// EOA path.
		balBytes := balance.Bytes()
		out := make([]byte, 1+8+1+len(balBytes))
		out[0] = byte(entityEOA)
		binary.BigEndian.PutUint64(out[1:9], nonce)
		out[9] = byte(len(balBytes))
		copy(out[10:], balBytes)
		return out
	}
	balBytes := balance.Bytes()
	size := 1 + 8 + 1 + len(balBytes) + 4 + len(code) + 4 + len(slots)*64
	out := make([]byte, 0, size)
	out = append(out, byte(entityContract))
	var nonceBuf [8]byte
	binary.BigEndian.PutUint64(nonceBuf[:], nonce)
	out = append(out, nonceBuf[:]...)
	out = append(out, byte(len(balBytes)))
	out = append(out, balBytes...)
	var codeLenBuf [4]byte
	binary.BigEndian.PutUint32(codeLenBuf[:], uint32(len(code)))
	out = append(out, codeLenBuf[:]...)
	out = append(out, code...)
	var slotCountBuf [4]byte
	binary.BigEndian.PutUint32(slotCountBuf[:], uint32(len(slots)))
	out = append(out, slotCountBuf[:]...)
	for _, s := range slots {
		out = append(out, s.Key[:]...)
		out = append(out, s.Value[:]...)
	}
	return out
}

// decodeEntity reverses encodeEntity. Blobs are produced internally and
// round-trip through streamsort, so malformed input is unreachable today; even
// so it returns an error rather than panicking, because it runs on the Stage A
// reader goroutine where a panic would bypass setErr and crash the pipeline.
func decodeEntity(blob []byte) (entity, error) {
	// need reports an error if blob lacks n bytes from pos onward.
	need := func(pos, n int) error {
		if pos+n > len(blob) {
			return fmt.Errorf("ethrex: truncated entity blob: need %d bytes at offset %d, have %d", n, pos, len(blob))
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
		return entity{}, fmt.Errorf("ethrex: unknown entity kind byte %d", blob[0])
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
	balBytes := blob[pos : pos+balLen]
	pos += balLen
	e.balance = new(uint256.Int)
	e.balance.SetBytes(balBytes)

	if e.kind == entityContract {
		if err := need(pos, 4); err != nil {
			return entity{}, err
		}
		codeLen := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
		pos += 4
		if err := need(pos, codeLen); err != nil {
			return entity{}, err
		}
		e.code = make([]byte, codeLen)
		copy(e.code, blob[pos:pos+codeLen])
		pos += codeLen
		if err := need(pos, 4); err != nil {
			return entity{}, err
		}
		slotCount := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
		pos += 4
		e.slots = make([]entitySlot, slotCount)
		for i := 0; i < slotCount; i++ {
			if err := need(pos, 64); err != nil {
				return entity{}, err
			}
			copy(e.slots[i].Key[:], blob[pos:pos+32])
			pos += 32
			copy(e.slots[i].Value[:], blob[pos:pos+32])
			pos += 32
		}
	}
	return e, nil
}
