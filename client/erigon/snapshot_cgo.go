//go:build cgo_erigon && cgo_erigon_commitment

package erigon

import (
	"context"
	"fmt"
	mrand "math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	internalerigon "github.com/ethereum/state-actor/internal/erigon"
	"github.com/ethereum/state-actor/internal/erigon/account"
	internalcommitment "github.com/ethereum/state-actor/internal/erigon/commitment"
	"github.com/ethereum/state-actor/internal/erigon/snap"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// fullRange is the SINGLE [0, 1) step-range for every domain: Erigon's
// DependencyIntegrityChecker only surfaces the frozen accounts/storage
// files if a commitment file with the EXACT same range exists.
//
// Continuability is the "fat genesis" construction (genesis_patch.go
// step 9 + the KeyCommitmentState anchor below): MaxTxNum[0]=StepSize-1
// makes genesis occupy the whole frozen step 0, so block 1 resumes at
// step 1 and its MDBX commitment WINS the getLatestFromDb EndTxNum gate
// instead of being shadowed by the frozen file (the block-2 "wrong trie
// root" fix — proven on stock erigon, cross-client root match). The
// bench daemon runs --snap.state.stop, freezing this layout.
var fullRange = snap.StepRange{From: 0, To: 1}

// erigonWorkers is the size of the Phase 1 autofill encode-worker pool.
// Defaults to min(NumCPU, 8) to match the proven cap from reth, besu, and
// nethermind, but is overridable via STATE_ACTOR_ERIGON_WORKERS to exploit
// many-core hosts (the RNG draw stays single-threaded on the main goroutine
// for cross-client invariance; only the CPU-bound encode is parallelised,
// so a larger pool is safe for the root).
var erigonWorkers = func() int {
	if v := os.Getenv("STATE_ACTOR_ERIGON_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}()

// recsplitWorkers sizes the parallel .kvi Build: min(NumCPU, 8) unless
// STATE_ACTOR_RECSPLIT_WORKERS overrides (1 = sequential).
func recsplitWorkers() int {
	if v := os.Getenv("STATE_ACTOR_RECSPLIT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	return n
}

// defaultMemoryLimit caps the Go heap at 8 GiB when GOMEMLIMIT is unset —
// the writer's live heap is ~2-5 GB and without any limit GOGC lets the
// peak double; this bounds it for an UNCONFIGURED end user. An explicit
// GOMEMLIMIT (any value) wins. Pebble arenas/caches are off-heap C malloc,
// invisible to this limit — they are bounded separately by the small
// memtable/cache defaults at the store creation sites.
var memLimitOnce sync.Once

func setDefaultMemoryLimit() {
	memLimitOnce.Do(func() {
		if os.Getenv("GOMEMLIMIT") == "" {
			debug.SetMemoryLimit(8 << 30)
		}
	})
}

// envCacheBytes returns the byte size from a "<N> GiB" env var, or
// defaultGB GiB if unset/invalid. Used to scale Pebble block caches to the
// host's RAM for the read-heavy commitment phase.
func envCacheBytes(envVar string, defaultGB int) int64 {
	gb := defaultGB
	if v := os.Getenv(envVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			gb = n
		}
	}
	return int64(gb) << 30
}

// entityWork is one alloc entry queued for an encode-worker. The main
// goroutine fills these and sends them on entityCh; workers consume
// and call encodeEntity.
type entityWork struct {
	addr  common.Address
	entry *allocAccount
}

// domainWrite is one (key, value) tuple ready to be Put into a specific
// streamsort. Sent from encode workers to per-domain writer goroutines.
type domainWrite struct {
	key   []byte
	value []byte
	// part is the commit-input sub-store index (0..15) for commitIn writes,
	// precomputed on the parallel encode worker (InputPart of the account
	// address). Unused for the accounts/storage/code channels.
	part uint8
}

// perDomainChans is the 4 per-domain BATCH channels — one dedicated writer
// goroutine drains each. Elements are []domainWrite batches (domainBatchSize
// rows) built by worker-local batchedSenders; 256 batches hold up to 8× the
// old 4096 per-item buffer's rows at ~1/128th the channel operations.
type perDomainChans struct {
	accounts chan []domainWrite
	storage  chan []domainWrite
	code     chan []domainWrite
	commitIn chan []domainWrite
}

// domainBatchSize is how many domainWrites accumulate worker-locally
// before ONE batched channel send. The profiled generation cost was the
// per-item channel machinery (selectgo ~30% cum across 48 producers),
// not the writers' pebble Puts — 128/batch cuts channel ops ~99%.
const domainBatchSize = 128

// batchedSender accumulates domainWrites worker-locally and ships them as
// []domainWrite batches. NOT goroutine-safe — one per (worker, domain).
type batchedSender struct {
	ctx context.Context
	ch  chan<- []domainWrite
	buf []domainWrite
}

func newBatchedSender(ctx context.Context, ch chan<- []domainWrite) *batchedSender {
	return &batchedSender{ctx: ctx, ch: ch, buf: make([]domainWrite, 0, domainBatchSize)}
}

func (s *batchedSender) send(dw domainWrite) error {
	s.buf = append(s.buf, dw)
	if len(s.buf) >= domainBatchSize {
		return s.flush()
	}
	return nil
}

// flush ships the buffered batch (no-op when empty). The buffer is handed
// off to the writer goroutine; a fresh one backs the next batch.
func (s *batchedSender) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	batch := s.buf
	s.buf = make([]domainWrite, 0, domainBatchSize)
	select {
	case s.ch <- batch:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// domainSenders is one producer goroutine's set of per-domain batched
// senders. Every producer (each encode worker; the main-goroutine PreAlloc
// drain) owns its own instance and MUST flushAll before finishing so no
// buffered rows are dropped.
type domainSenders struct {
	accounts *batchedSender
	storage  *batchedSender
	code     *batchedSender
	commitIn *batchedSender
}

func newDomainSenders(ctx context.Context, chans *perDomainChans) *domainSenders {
	return &domainSenders{
		accounts: newBatchedSender(ctx, chans.accounts),
		storage:  newBatchedSender(ctx, chans.storage),
		code:     newBatchedSender(ctx, chans.code),
		commitIn: newBatchedSender(ctx, chans.commitIn),
	}
}

func (s *domainSenders) flushAll() error {
	for _, b := range []*batchedSender{s.accounts, s.storage, s.code, s.commitIn} {
		if err := b.flush(); err != nil {
			return err
		}
	}
	return nil
}

// domainCounts tracks per-domain entry counts. Workers increment via
// atomic.AddUint64. Phase 5 reads via plain access AFTER all workers
// have drained (sync.WaitGroup.Wait provides the happens-before
// barrier).
type domainCounts struct {
	accounts uint64
	storage  uint64
	code     uint64
}

// writeSnapshots is the multi-range streaming snapshot orchestrator
// with a Phase 1 channel-pipeline encode-worker pool and Phase 3
// WaitGroup fan-out across the 12 (range, domain) tuples.
//
// Memory bounded by streamsort working set (~4 stores × ~256 MiB
// memtable = ~1 GB) regardless of --target-size. Disk usage scales
// with the autofill payload (~20 GB at 25 GB target), landing under
// <cfg.DBPath>/streamsort-<domain>/ on the bind-mounted filesystem.
//
// Phases:
//  1. Open 4 streamsorts (accounts/storage/code/commitmentInputs)
//     under cfg.DBPath.
//  2. Spawn N encode workers (N = erigonWorkers) + 4 per-domain
//     writer goroutines (one per streamsort).
//  3. Main goroutine drains foundational.Spec + runs the AutoFill
//     RNG loop, sending entityWork records to entityCh. Workers
//     consume, encode SerialiseV3 + commitment-update, emit
//     (key, value) tuples to per-domain channels. Writer goroutines
//     drain channels into streamsort.Put. CPU encode and disk I/O
//     overlap; RNG order stays on main thread for cross-client
//     invariance.
//  4. Fold the commitment: the default Direct-Drive Fold streams the
//     hashed-keyed sub-stores straight into the vendored engine (the
//     Updates/etl engine path remains behind STATE_ACTOR_COMMITMENT_DIRECT=0).
//     Branch rows are retained on the Result for direct streaming.
//     5b. Snapshot writes FAN OUT into goroutines (semaphore-bounded at
//     NumCPU): accounts/storage/code start BEFORE the fold (overlap);
//     commitment queues after it, streaming Result.BranchIterate straight
//     into WriteCommitment (no intermediate branchesStore re-sort).
//
// Returns the HPH root; runImpl patches it into block-0 header.stateRoot.
func writeSnapshots(
	ctx context.Context,
	cfg generator.Config,
	foundational *FoundationalAlloc,
	stats *generator.Stats,
) (common.Hash, error) {
	setDefaultMemoryLimit()
	// -- Step 1: open 4 streamsorts under cfg.DBPath (bind-mounted disk).
	// 128 MiB memtables (vs the 256 MiB default): bulk sequential writes
	// drained by one sequential scan; the arenas are off-heap C malloc —
	// committed RSS an unconfigured end user pays.
	valueOpts := streamsort.Options{MemTableBytes: 128 << 20}
	accountsStore, err := streamsort.NewWithOptions(cfg.DBPath, valueOpts)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open accounts streamsort: %w", err)
	}
	defer accountsStore.Close()

	storageStore, err := streamsort.NewWithOptions(cfg.DBPath, valueOpts)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open storage streamsort: %w", err)
	}
	defer storageStore.Close()

	codeStore, err := streamsort.NewWithOptions(cfg.DBPath, valueOpts)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open code streamsort: %w", err)
	}
	defer codeStore.Close()

	// 16 nibble-partitioned commit-input sub-stores; each of the 16 fold
	// workers reads exactly ONE (disjoint block caches, no cross-worker
	// contention). Cache sizing by read pattern: hashed keying (the default
	// — DDF cursors, or the engine/hashed reused SeekGE Getter) reads each
	// store ONCE in ascending order, so the cache is readahead-only and
	// 8 MiB/store suffices; only the chunked PLAIN path does ~billions of
	// random Gets and needs the big cache (4 GiB default). An explicit
	// STATE_ACTOR_COMMITMENT_CACHE_GB always wins (opt-in for more RAM —
	// the arenas/caches are off-heap C malloc, committed RSS by default).
	partCache := int64(8 << 20)
	if os.Getenv("STATE_ACTOR_COMMITMENT_CACHE_GB") != "" {
		partCache = envCacheBytes("STATE_ACTOR_COMMITMENT_CACHE_GB", 4) / int64(internalcommitment.NumInputParts)
		if partCache < 8<<20 {
			partCache = 8 << 20
		}
	}
	commitInOpts := streamsort.Options{BlockCacheBytes: partCache, MemTableBytes: 64 << 20}
	commitmentInputStores := make([]*streamsort.Store, internalcommitment.NumInputParts)
	for i := range commitmentInputStores {
		s, serr := streamsort.NewWithOptions(cfg.DBPath, commitInOpts)
		if serr != nil {
			for _, s2 := range commitmentInputStores {
				if s2 != nil {
					s2.Close()
				}
			}
			return common.Hash{}, fmt.Errorf("writeSnapshots: open commitmentInput sub-store %d: %w", i, serr)
		}
		commitmentInputStores[i] = s
	}
	defer func() {
		for _, s := range commitmentInputStores {
			if s != nil {
				s.Close()
			}
		}
	}()

	// counts: atomic per-domain increments by workers.
	var counts domainCounts

	// -- Step 2: spawn worker pool + per-domain writer goroutines.
	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()

	// 256 in-flight BATCHES (× domainBatchSize rows) per domain — same
	// order of buffered rows as the old 4096 per-item buffer, ~1/128th the
	// channel operations.
	chans := &perDomainChans{
		accounts: make(chan []domainWrite, 256),
		storage:  make(chan []domainWrite, 256),
		code:     make(chan []domainWrite, 256),
		commitIn: make(chan []domainWrite, 256),
	}

	N := erigonWorkers
	entityCh := make(chan entityWork, 2*N)
	encodeErrCh := make(chan error, N)
	writerErrCh := make(chan error, 4)

	var encodeWg sync.WaitGroup
	encodeWg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer encodeWg.Done()
			if err := autofillEncodeWorker(pipelineCtx, entityCh, chans, &counts); err != nil {
				select {
				case encodeErrCh <- err:
				default:
				}
				cancelPipeline()
			}
		}()
	}

	var writerWg sync.WaitGroup
	writerWg.Add(4)
	go runDomainWriter(pipelineCtx, &writerWg, writerErrCh, cancelPipeline, chans.accounts, accountsStore, "accounts")
	go runDomainWriter(pipelineCtx, &writerWg, writerErrCh, cancelPipeline, chans.storage, storageStore, "storage")
	go runDomainWriter(pipelineCtx, &writerWg, writerErrCh, cancelPipeline, chans.code, codeStore, "code")
	go runCommitInWriter(pipelineCtx, &writerWg, writerErrCh, cancelPipeline, chans.commitIn, commitmentInputStores)

	// sendEntity exits early if the pipeline already cancelled (a worker
	// or writer failed mid-stream).
	sendEntity := func(ew entityWork) error {
		select {
		case entityCh <- ew:
			return nil
		case <-pipelineCtx.Done():
			return pipelineCtx.Err()
		}
	}

	// -- Step 3a: drain foundational.Spec → entityCh.
	// Also build genesisAddrs for the dedup-redraw loop (matches geth
	// byte-for-byte: dedup against spec only, never against other
	// autofill draws — see client/geth/state_writer.go:120-148).
	genesisAddrs := make(map[common.Address]struct{}, len(foundational.Spec))
	var pipelineErr error
	for addr, entry := range foundational.Spec {
		genesisAddrs[addr] = struct{}{}
		if err := sendEntity(entityWork{addr: addr, entry: entry}); err != nil {
			pipelineErr = err
			break
		}
	}

	// -- Step 3b: AutoFill RNG loop — STREAMING (main thread only).
	//
	// The dedup-redraw loop MUST match client/geth/state_writer.go:116-148
	// byte-for-byte: geth burns RNG draws on collision until it finds a
	// non-colliding address, with no nil-checks. Any nil-check break here
	// would skip the assignment but still advance the RNG, desynchronizing
	// from geth's draw sequence and producing a different alloc + a
	// different cross-client genesis state-root.
	//
	// AutoFill.Draw{EOA,Contract} return non-nil for all valid plans (a
	// nil return would crash geth at the same point — we mirror that
	// contract rather than guard against it).
	if pipelineErr == nil && cfg.AutoFill != nil {
		// Erigon never reads the drawn Account's AddrHash/CodeHash (it keys
		// stores by plain address and re-derives the code hash on the parallel
		// encode workers — see encodeEntity), so skip those keccaks on this
		// single-threaded draw loop. RNG-draw-sequence-neutral (pinned by
		// TestFlavoredDrawRNGSequenceInvariant); set on a LOCAL copy so the
		// shared Plan is never mutated.
		autoFill := *cfg.AutoFill
		autoFill.SkipDerivedHashes = true
		rng := mrand.New(mrand.NewSource(cfg.Seed))
		cfg.Progress.Stage("erigon: generating accounts")
		for i := 0; i < autoFill.NumEOAs && pipelineErr == nil; i++ {
			acc := autoFill.DrawEOA(rng)
			for _, dup := genesisAddrs[acc.Address]; dup; {
				acc = autoFill.DrawEOA(rng)
				_, dup = genesisAddrs[acc.Address]
			}
			entry := &allocAccount{Nonce: acc.StateAccount.Nonce}
			if acc.StateAccount.Balance != nil {
				entry.Balance = acc.StateAccount.Balance.ToBig()
			}
			// EIP-7702 delegation marker: ~30% of EOAs from
			// internal/autofill/eoa_flavor.go have HasDelegation set,
			// which puts a 23-byte 0xef0100||target20 in acc.Code with
			// CodeHash = keccak256(code). The geth writer honors this
			// at state_writer.go:127-130; we MUST mirror it here or
			// the resulting CodeHash diverges from MPT for ~30% of
			// EOAs → wrong genesis state root vs cross-client peers.
			// Bug surfaced by cross-client bench on 2026-06-03:
			// geth+reth 0x7fa5f44... vs erigon 0xcbab49... at SEED=42.
			if len(acc.Code) > 0 {
				entry.Code = acc.Code
				stats.CodeBytes += uint64(len(acc.Code))
			}
			if err := sendEntity(entityWork{addr: acc.Address, entry: entry}); err != nil {
				pipelineErr = err
				break
			}
			stats.AccountsCreated++
			cfg.Progress.Tick(int64(i+1), int64(autoFill.NumEOAs), "EOAs")
		}
		for i := 0; i < autoFill.NumContracts && pipelineErr == nil; i++ {
			c := autoFill.DrawContract(rng)
			for _, dup := genesisAddrs[c.Address]; dup; {
				c = autoFill.DrawContract(rng)
				_, dup = genesisAddrs[c.Address]
			}
			entry := &allocAccount{Nonce: c.StateAccount.Nonce}
			if c.StateAccount.Balance != nil {
				entry.Balance = c.StateAccount.Balance.ToBig()
			}
			if len(c.Code) > 0 {
				entry.Code = c.Code
				stats.CodeBytes += uint64(len(c.Code))
			}
			if len(c.Storage) > 0 {
				entry.Storage = make(map[common.Hash]common.Hash, len(c.Storage))
				for _, s := range c.Storage {
					entry.Storage[s.Key] = s.Value
				}
			}
			if err := sendEntity(entityWork{addr: c.Address, entry: entry}); err != nil {
				pipelineErr = err
				break
			}
			stats.StorageSlotsCreated += len(c.Storage)
			stats.StorageBytes += uint64(len(c.Storage)) * 64
			stats.ContractsCreated++
			cfg.Progress.Tick(int64(i+1), int64(autoFill.NumContracts), "contracts")
		}
	}
	stats.TotalBytes = stats.StorageBytes + stats.CodeBytes

	// Close entityCh → workers drain remaining items then exit.
	close(entityCh)
	encodeWg.Wait()

	cfg.Progress.Stage("erigon: building snapshots & commitment")

	// -- Step 3c: drain spec-PreAlloc Storage iters into storage +
	// commitment-input streamsorts.
	//
	// cfg.PreAlloc entries with Storage iter.Seq2 (e.g., bloatnet's
	// `template: erc20` contracts) are NOT drained by materializePreAlloc
	// (generator/config.go:130-165 comment "Storage is NOT drained — it
	// stays as iter.Seq2 on c.PreAlloc"). The geth and reth writers each
	// run a Phase 0 that drains the iter (client/geth/state_writer.go:554
	// runPhase0 + client/reth/spec_storage_streaming_cgo.go:81
	// streamSpecStorage). state-actor's erigon writer previously dropped
	// these slots (~7M at SPEC_TARGET_GB=1) → genesis state root diverged
	// from cross-client peers (geth/reth 0x7fa5f44... vs erigon 0xcbab49...
	// on 2026-06-03).
	//
	// We run after encodeWg.Wait() so the worker pool is fully drained
	// and the only writers to chans.storage / chans.commitIn are this
	// goroutine — no cross-goroutine ordering concerns. The chans still
	// have their per-domain writer goroutines reading (they exit on
	// close(chans.*) below).
	if pipelineErr == nil {
		// Count-only heartbeat for the spec-storage drain — the dominant phase
		// on bloat specs (millions of slots), otherwise silent under the
		// "building snapshots & commitment" banner above. Single goroutine here,
		// so one SlotWorker; nil-safe when progress is unwired.
		slotW := cfg.Progress.SlotMeter().Worker()
		// This main-goroutine producer gets its own batched senders —
		// flushed after the drain, BEFORE the channels close below.
		drainSenders := newDomainSenders(pipelineCtx, chans)
		for i := range cfg.PreAlloc {
			pe := &cfg.PreAlloc[i]
			if pe.Storage == nil {
				continue
			}
			part := uint8(internalcommitment.InputPart(pe.Address[:]))
			pe.Storage(func(slot, value common.Hash) bool {
				if err := encodeStorageSlot(pipelineCtx, pe.Address, slot, value, part, drainSenders, &counts); err != nil {
					pipelineErr = err
					return false
				}
				slotW.Slot()
				// trimLeadingZeros may filter zero values; conservatively
				// count by the unfiltered slot. Storage byte tally for
				// stats includes raw 32-byte value capacity (matching
				// geth's accounting in state_writer.go runPhase0).
				stats.StorageSlotsCreated++
				stats.StorageBytes += 64
				return true
			})
			if pipelineErr != nil {
				break
			}
		}
		// Ship the drain's partially-filled batches before the channels
		// close below — dropping them would silently lose rows.
		if pipelineErr == nil {
			if err := drainSenders.flushAll(); err != nil {
				pipelineErr = err
			}
		}
		stats.TotalBytes = stats.StorageBytes + stats.CodeBytes
	}

	// Close per-domain channels → writers drain then exit.
	close(chans.accounts)
	close(chans.storage)
	close(chans.code)
	close(chans.commitIn)
	writerWg.Wait()

	// Surface the first encountered error. Worker errors take priority
	// over the local pipelineErr (which is just ctx.Canceled in the
	// cancel path; the real cause is in encodeErrCh or writerErrCh).
	select {
	case err := <-encodeErrCh:
		return common.Hash{}, fmt.Errorf("writeSnapshots: encode worker: %w", err)
	default:
	}
	select {
	case err := <-writerErrCh:
		return common.Hash{}, fmt.Errorf("writeSnapshots: domain writer: %w", err)
	default:
	}
	if pipelineErr != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: pipeline: %w", pipelineErr)
	}

	// Finalize the 4 streamsorts. After this they enter their FINALIZED
	// state and Get/Iterate become safe for concurrent callers — Phase 2
	// (ConcurrentPatriciaHashed, 16 workers) does concurrent
	// commitmentInputStore.Get; Phase 5b (12-way WriteDomain fan-out)
	// does concurrent accountsStore/storageStore/codeStore.Iterate.
	// Without Finalize, the post-Phase-1 batch flush would race against
	// the next call's batch.Commit and trigger pebble: batch already
	// committing. Finalize moves the flush to a one-shot mutex-serialized
	// transition so the read path is lock-free.
	for _, fz := range []struct {
		name  string
		store *streamsort.Store
	}{
		{"accounts", accountsStore},
		{"storage", storageStore},
		{"code", codeStore},
	} {
		if err := fz.store.Finalize(); err != nil {
			return common.Hash{}, fmt.Errorf("writeSnapshots: Finalize %s streamsort: %w", fz.name, err)
		}
	}
	for i, s := range commitmentInputStores {
		if err := s.Finalize(); err != nil {
			return common.Hash{}, fmt.Errorf("writeSnapshots: Finalize commitmentInput sub-store %d: %w", i, err)
		}
	}

	// -- Step 4: the commitment fold over the commit-input sub-stores.
	// Branch rows are RETAINED on the returned Result (DDF: per-worker
	// write-once sinks; engine fallback: the live branch store) and
	// WriteCommitment streams Result.BranchIterate directly — the old
	// intermediate branchesStore re-sort no longer exists.

	// Overlap the Phase-5b accounts/storage/code snapshot-write with the
	// commitment fold below. These three domains depend only on the finalized
	// accountsStore/storageStore/codeStore + counts — NOT on the fold's result —
	// so start them NOW and let their (mostly single-core) compression run on
	// the cores the 16-way fold leaves idle. Only WriteCommitment needs the
	// fold's branches + keyStateValue, so it is queued after the fold. The
	// Writer is immutable and every domain writes its own files, so all these
	// goroutines are race-free.
	settings := snap.Settings{
		Seed:              cfg.Seed,
		StepSize:          internalerigon.StepSize,
		StepsInFrozenFile: internalerigon.StepsInFrozenFile,
		SnapshotVersion:   internalerigon.SnapshotFormatVersion,
		// Parallel .kvi Build (byte-identical at any count). Default
		// min(NumCPU, 8): it runs at the very tail when the value-domain
		// writers have drained. STATE_ACTOR_RECSPLIT_WORKERS overrides.
		RecSplitWorkers: recsplitWorkers(),
	}
	w, err := snap.NewWriter(cfg.DBPath, settings)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: snap.NewWriter: %w", err)
	}
	defer w.Close()

	type domainSpec struct {
		domain snap.Domain
		store  *streamsort.Store
		count  uint64
	}
	domainSpecs := []domainSpec{
		{snap.DomainAccounts, accountsStore, counts.accounts},
		{snap.DomainStorage, storageStore, counts.storage},
		{snap.DomainCode, codeStore, counts.code},
	}
	emitErrCh := make(chan error, len(domainSpecs)+1) // +1 for commitment
	var emitWg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, ds := range domainSpecs {
		sem <- struct{}{}
		emitWg.Add(1)
		go func(ds domainSpec) {
			defer func() { <-sem; emitWg.Done() }()
			start := time.Now()
			if err := w.WriteDomain(ctx, ds.domain, fullRange, ds.count,
				snap.FromStreamsort(ds.store)); err != nil {
				select {
				case emitErrCh <- fmt.Errorf("WriteDomain(%v): %w", ds.domain, err):
				default:
				}
			}
			if cfg.Verbose {
				fmt.Printf("client/erigon: timing WriteDomain(%v) %s\n", ds.domain, time.Since(start).Round(time.Second))
			}
		}(ds)
	}

	// cfg.DBPath (real bind-mounted disk) hosts the branch sinks/store and,
	// on the engine fallback, the etl spill; "" would put tens of GB of
	// scratch on tmpfs (RAM) on the bench host.
	keying := internalcommitment.KeyingHashed
	foldStart := time.Now()
	result, err := internalcommitment.ComputeGenesisRoot(commitmentInputStores, cfg.DBPath, keying)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: ComputeGenesisRoot: %w", err)
	}
	if cfg.Verbose {
		fmt.Printf("client/erigon: timing fold %s\n", time.Since(foldStart).Round(time.Second))
	}
	// The retained branch source outlives the fold. The defer is safe on
	// every path because its only reader — the WriteCommitment goroutine —
	// is spawned AFTER the early error returns below and is joined by
	// emitWg.Wait() before the success return.
	defer result.CloseBranches()
	// Close the 16 commit-input sub-stores NOW (not at function scope):
	// their caches/memtables/spill dirs are dead weight during the
	// mmap-heavy Phase 5b — holding them once pushed a 100 GB run to the
	// OOM edge. Close is idempotent, so the deferred Close is a no-op.
	closeStart := time.Now()
	for i, s := range commitmentInputStores {
		if err := s.Close(); err != nil {
			return common.Hash{}, fmt.Errorf("writeSnapshots: close commitmentInput sub-store %d: %w", i, err)
		}
	}
	if cfg.Verbose {
		fmt.Printf("client/erigon: timing commitIn close %s\n", time.Since(closeStart).Round(time.Second))
	}
	nBranches := result.BranchCount
	// KeyCommitmentState anchor: (txNum=StepSize-1, blockNum=0) — the fat
	// genesis. SeekCommitment reads this and resumes block 1 at step 1,
	// above the frozen [0,1) range (see fullRange). Changing these two
	// numbers WILL re-break the block-2 stall.
	keyStateValue, err := internalcommitment.EncodeKeyCommitmentStateValue(internalerigon.StepSize-1, 0, result.HPHState)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: encode KeyCommitmentState: %w", err)
	}

	// -- Step 5b: accounts/storage/code are already being written (fan-out
	// started above, overlapping the fold). Queue the commitment domain now
	// that the fold has produced its branches + keyStateValue.
	//
	// Commitment: single [0,1) range. It is the LARGEST domain (~44 GB .kv at
	// 100 GB, plus the only recsplit MPHF over ~nBranches keys) yet depends
	// only on the fold's retained branch store + keyStateValue — NOT on the
	// other domains. Run it as a 4th goroutine in the SAME fan-out (the Writer
	// is immutable and each domain writes its own files) so its long build
	// overlaps accounts/storage/code instead of running serially after them.
	// The branch rows stream STRAIGHT from the walk's live branch store
	// (Result.BranchIterate is already ascending; WriteCommitment splices the
	// KeyCommitmentState row at its sort position) — no intermediate re-sort
	// store. frozenSteps(commitment)=0 so the daemon's mem-tier
	// KeyCommitmentState writes pass CheckDataAvailable trivially (see the
	// fullRange doc above).
	sem <- struct{}{}
	emitWg.Add(1)
	go func() {
		defer func() { <-sem; emitWg.Done() }()
		start := time.Now()
		if err := snap.WriteCommitment(ctx, w, fullRange, keyStateValue, snap.BranchStream(result.BranchIterate), nBranches); err != nil {
			select {
			case emitErrCh <- fmt.Errorf("WriteCommitment: %w", err):
			default:
			}
		}
		if cfg.Verbose {
			fmt.Printf("client/erigon: timing WriteCommitment %s\n", time.Since(start).Round(time.Second))
		}
	}()
	emitWg.Wait()
	select {
	case err := <-emitErrCh:
		return common.Hash{}, fmt.Errorf("writeSnapshots: %w", err)
	default:
	}

	if cfg.Verbose {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Printf("client/erigon: memory: go_heap=%.1fGiB go_sys=%.1fGiB (pebble arenas/caches are off-heap C malloc; mmapped .kv pages are kernel-reclaimable — RSS overstates committed use)\n",
			float64(ms.HeapAlloc)/(1<<30), float64(ms.Sys)/(1<<30))
		fmt.Printf("client/erigon: wrote snapshots: spec=%d autofill_accounts=%d contracts=%d storage_slots=%d branches=%d workers=%d root=%s\n",
			len(foundational.Spec), stats.AccountsCreated, stats.ContractsCreated, stats.StorageSlotsCreated, nBranches, N, result.Root.Hex())
		fmt.Printf("client/erigon: domain entry counts: accounts=%d storage=%d code=%d\n",
			counts.accounts, counts.storage, counts.code)
	}

	return result.Root, nil
}

// runDomainWriter is the goroutine entry-point for one per-domain
// writer. Drains `in` into `store` until either the channel closes
// (clean shutdown) or ctx cancels (error path). On error, pushes to
// errCh and triggers cancel.
func runDomainWriter(
	ctx context.Context,
	wg *sync.WaitGroup,
	errCh chan<- error,
	cancel context.CancelFunc,
	in <-chan []domainWrite,
	store *streamsort.Store,
	label string,
) {
	defer wg.Done()
	if err := domainWriter(ctx, in, store, label); err != nil {
		select {
		case errCh <- err:
		default:
		}
		cancel()
	}
}

// autofillEncodeWorker consumes entityWork from entityCh, encodes
// each entry on a worker goroutine, and emits (key, value) tuples to
// the per-domain channels. Exits cleanly on entityCh close OR ctx
// cancellation.
func autofillEncodeWorker(
	ctx context.Context,
	in <-chan entityWork,
	out *perDomainChans,
	counts *domainCounts,
) error {
	senders := newDomainSenders(ctx, out)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ew, ok := <-in:
			if !ok {
				// Ship the partially-filled batches before exiting —
				// dropping them would silently lose rows.
				return senders.flushAll()
			}
			if err := encodeEntity(ctx, ew, senders, counts); err != nil {
				return err
			}
		}
	}
}

// encodeEntity is the worker-side encode for one alloc entry. Emits:
//   - 1 account row → accounts channel (always)
//   - 1 code row → code channel (only if entity has bytecode)
//   - len(entry.Storage) storage rows → storage + commitIn channels
//   - 1 commitment-input row → commitIn channel (account-level update)
//
// All composite keys + encoded values are computed here (CPU-bound).
// The per-domain writer goroutines absorb the disk-bound Put.
func encodeEntity(
	ctx context.Context,
	ew entityWork,
	out *domainSenders,
	counts *domainCounts,
) error {
	// Snapshot keys are plain addr (20 bytes) — no rangeIdx prefix in
	// the single-tier [0,1) layout.
	addrKey := make([]byte, 20)
	copy(addrKey, ew.addr[:])

	// Accounts snapshot value: SerialiseV3.
	acct := account.Account{
		Nonce:    ew.entry.Nonce,
		CodeHash: account.EmptyCodeHash,
	}
	var balance *uint256.Int
	if ew.entry.Balance != nil {
		b, overflow := uint256.FromBig(ew.entry.Balance)
		if overflow {
			return fmt.Errorf("encodeEntity: balance overflow for %s", ew.addr.Hex())
		}
		acct.Balance = *b
		balance = b
	}
	// Hash the code ONCE here (for the snapshot CodeHash) and reuse it for the
	// commitment Update below — EncodeAccountUpdate would otherwise keccak the
	// same bytes a second time (commitment.go). nil = no code.
	var codeHash *common.Hash
	if len(ew.entry.Code) > 0 {
		h := crypto.Keccak256Hash(ew.entry.Code)
		copy(acct.CodeHash[:], h[:])
		codeHash = &h
	}
	if err := out.accounts.send(domainWrite{key: addrKey, value: account.SerialiseV3(acct)}); err != nil {
		return err
	}
	atomic.AddUint64(&counts.accounts, 1)

	// Code snapshot value: raw bytecode keyed by addr.
	if len(ew.entry.Code) > 0 {
		if err := out.code.send(domainWrite{key: addrKey, value: ew.entry.Code}); err != nil {
			return err
		}
		atomic.AddUint64(&counts.code, 1)
	}

	// Route this account (and all its storage) to its commit-input sub-store
	// by the first nibble of keccak(addr) — the fold's worker shard. The row
	// is KEYED by the full hashed key with the plain key in the value
	// (EncodeInputRow), giving the fold sequential reads; the keccak stays on
	// this parallel encode worker.
	hashedAddr := internalcommitment.HashedKey(addrKey)
	part := hashedAddr[0]

	// Inline storage (foundational PreAlloc + contract autofill).
	for slot, value := range ew.entry.Storage {
		if err := encodeStorageSlot(ctx, ew.addr, slot, value, part, out, counts); err != nil {
			return err
		}
	}

	// Commitment input: account-level Update. Reuse the code hash computed
	// above (no second keccak).
	commitBytes := internalcommitment.EncodeAccountUpdateCodeHash(ew.entry.Nonce, balance, codeHash)
	return out.commitIn.send(domainWrite{key: hashedAddr, value: internalcommitment.EncodeInputRow(addrKey, commitBytes), part: part})
}

// encodeStorageSlot encodes one (addr, slot, value) tuple. Skip on
// all-zero value (Erigon's StorageDomain treats absent ≡ zero;
// storing zero is wrong).
func encodeStorageSlot(
	ctx context.Context,
	addr common.Address,
	slotKey common.Hash,
	slotValue common.Hash,
	part uint8, // commit-input sub-store = the owning account's part
	out *domainSenders,
	counts *domainCounts,
) error {
	trimmed := trimLeadingZeros(slotValue[:])
	if len(trimmed) == 0 {
		return nil
	}
	// Plain key (addr || slot = 52 bytes) for both snapshot and
	// commitment-input. No rangeIdx prefix in the single-tier [0,1)
	// layout — same key shape for both downstream consumers.
	plainKey := make([]byte, 0, 20+32)
	plainKey = append(plainKey, addr[:]...)
	plainKey = append(plainKey, slotKey[:]...)
	if err := out.storage.send(domainWrite{key: plainKey, value: trimmed}); err != nil {
		return err
	}
	atomic.AddUint64(&counts.storage, 1)

	commitBytes := internalcommitment.EncodeStorageUpdate(slotValue[:])
	// Same part as the owning account: HashedKey(addr||slot)[0] derives
	// from keccak(addr).
	return out.commitIn.send(domainWrite{key: internalcommitment.HashedKey(plainKey), value: internalcommitment.EncodeInputRow(plainKey, commitBytes), part: part})
}

// domainWriter drains a single per-domain channel into the
// corresponding streamsort. streamsort is single-writer by design
// (internal/streamsort/streamsort.go) — having exactly one writer
// goroutine per domain is what makes the worker pool safe without a
// mutex on the streamsort itself.
// runCommitInWriter drains the commit-input channel into the 16 nibble sub-stores,
// routing each write by its precomputed part (set in encodeEntity, so the keccak
// stays on the parallel encode workers, not this single writer).
func runCommitInWriter(
	ctx context.Context,
	wg *sync.WaitGroup,
	errCh chan<- error,
	cancel context.CancelFunc,
	in <-chan []domainWrite,
	stores []*streamsort.Store,
) {
	defer wg.Done()
	if err := commitInWriter(ctx, in, stores); err != nil {
		select {
		case errCh <- err:
		default:
		}
		cancel()
	}
}

func commitInWriter(ctx context.Context, in <-chan []domainWrite, stores []*streamsort.Store) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch, ok := <-in:
			if !ok {
				return nil
			}
			for _, dw := range batch {
				if err := stores[dw.part].Put(dw.key, dw.value); err != nil {
					return fmt.Errorf("commitInWriter[part=%d]: %w", dw.part, err)
				}
			}
		}
	}
}

func domainWriter(
	ctx context.Context,
	in <-chan []domainWrite,
	store *streamsort.Store,
	label string,
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch, ok := <-in:
			if !ok {
				return nil
			}
			for _, dw := range batch {
				if err := store.Put(dw.key, dw.value); err != nil {
					return fmt.Errorf("domainWriter[%s]: %w", label, err)
				}
			}
		}
	}
}

// trimLeadingZeros returns the suffix of b after the longest run of
// leading zero bytes. Returns an empty slice for an all-zero input
// (matches Erigon's storage-domain "absent = zero" semantics).
func trimLeadingZeros(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}
