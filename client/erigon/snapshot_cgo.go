//go:build cgo_erigon && cgo_erigon_commitment

package erigon

import (
	"context"
	"fmt"
	mrand "math/rand"
	"runtime"
	"sync"
	"sync/atomic"

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

// fullRange is the SINGLE [0, 1) step-range we emit for every domain
// (accounts/storage/code/commitment). The [0, 1) range is REQUIRED by
// Erigon's DependencyIntegrityChecker (state_schema.go:69-70 +
// entity_integrity_check.go:133-155): the frozen accounts.0-1 /
// storage.0-1 files are only made visible if a commitment file with
// the EXACT same range exists, so all 4 domains MUST share [0, 1).
//
// Continuability is solved by the "fat genesis" construction (see
// genesis_patch.go step 9 + the commitment anchor below), NOT by the
// file range. We write MaxTxNum[0]=StepSize-1 so genesis OCCUPIES the
// entire frozen step 0, and anchor KeyCommitmentState at
// txNum=StepSize-1. On boot, SeekCommitment resumes the first live
// block (block 1) at txNum=StepSize — i.e. STEP 1, one step ABOVE the
// frozen [0, 1) range. Block 1's commitment then writes to the MDBX hot
// tier at step 1, which WINS the getLatestFromDb EndTxNum gate
// (domain.go:1582: an MDBX step-S write beats the frozen file iff
// lastTxNumOfStep(S) >= files.EndTxNum()=StepSize, i.e. S>=1) instead
// of being shadowed. This is the no-patch fix for the block-2 "wrong
// trie root" stall — keep the bloat in flat files, keep only the
// advancing commitment in MDBX (proven on STOCK erigon 2026-06-18:
// chain advanced past block 70, erigon==geth==reth root).
//
// CheckDataAvailable (commitmentdb/reader.go:31) still passes:
// frozenSteps([0,1))=0, and the live step (1) is not < 0. The
// replaceShortenedKeysInBranch shard-size short-circuit
// (domain_committed.go:54, shardSize<2) also still holds.
//
// Cross-client genesis-state-root invariance is preserved because the
// HPH root is content-addressed over the alloc, independent of file
// partitioning AND of the txNum bookkeeping
// (internal/erigon/commitment/commitment.go::ComputeGenesisRoot has no
// file-layout parameter; the fat-genesis txNum changes are pure
// bookkeeping that never touch state values).
//
// State-actor's bench launches the daemon with `--snap.state.stop`
// which sets ProduceE3=false (aggregator.go:1999-2027 early-returns
// at :2023), freezing the [0, 1) layout — no merge or collation will
// run, preserving the file forever.
var fullRange = snap.StepRange{From: 0, To: 1}

// erigonWorkers is the size of the Phase 1 autofill encode-worker pool.
// Defaults to min(NumCPU, 8) to match the proven cap from reth, besu,
// and nethermind (client/reth/spec_storage_streaming_cgo.go:95-104,
// client/besu/state_writer_cgo.go:298, client/nethermind/phase0_cgo.go).
var erigonWorkers = func() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}()

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
}

// perDomainChans is the 4 per-domain channels — one dedicated writer
// goroutine drains each. The 4096 buffer absorbs a single
// 615-slot-contract burst without blocking the encoder.
type perDomainChans struct {
	accounts chan domainWrite
	storage  chan domainWrite
	code     chan domainWrite
	commitIn chan domainWrite
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
//  4. Run HPH commitment over the commitmentInputStore (disk-backed
//     ctx.Account/Storage callbacks via streamsort.Get).
//     5a. Marshal branches map into branchesStore (sequential — small).
//     5b. Multi-range write loop — 4 ranges × 3 domains FAN OUT into
//     goroutines (semaphore-bounded at NumCPU). Commitment phase
//     stays serial (small + shared branchesStore).
//
// Returns the HPH root; runImpl patches it into block-0 header.stateRoot.
func writeSnapshots(
	ctx context.Context,
	cfg generator.Config,
	foundational *FoundationalAlloc,
	stats *generator.Stats,
) (common.Hash, error) {
	// -- Step 1: open 4 streamsorts under cfg.DBPath (bind-mounted disk).
	accountsStore, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open accounts streamsort: %w", err)
	}
	defer accountsStore.Close()

	storageStore, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open storage streamsort: %w", err)
	}
	defer storageStore.Close()

	codeStore, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open code streamsort: %w", err)
	}
	defer codeStore.Close()

	// commitmentInputStore takes the heavy random-read workload: the
	// 16 ConcurrentPatriciaHashed workers call subtreeCtx.Storage /
	// Account on every leaf (~344M Get calls at SPEC_TARGET_GB=1 for
	// a 12 GiB store, ~1.7B at 25 GiB). With the default 8 MiB block
	// cache the LSM SSTs miss on most reads. Bump the cache here so
	// the Pebble block cache holds a non-trivial fraction of the
	// working set; benchmarking showed 50+ min Phase 2 wall at default.
	// Tunable via the environment-derived future Options if needed;
	// 4 GiB is the floor that the bench host (240 GiB RAM) can spare.
	commitmentInputStore, err := streamsort.NewWithOptions(cfg.DBPath, streamsort.Options{
		BlockCacheBytes: 4 << 30,
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open commitmentInput streamsort: %w", err)
	}
	defer commitmentInputStore.Close()

	// counts: atomic per-domain increments by workers.
	var counts domainCounts

	// -- Step 2: spawn worker pool + per-domain writer goroutines.
	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()

	chans := &perDomainChans{
		accounts: make(chan domainWrite, 4096),
		storage:  make(chan domainWrite, 4096),
		code:     make(chan domainWrite, 4096),
		commitIn: make(chan domainWrite, 4096),
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
	go runDomainWriter(pipelineCtx, &writerWg, writerErrCh, cancelPipeline, chans.commitIn, commitmentInputStore, "commitmentInput")

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
		rng := mrand.New(mrand.NewSource(cfg.Seed))
		for i := 0; i < cfg.AutoFill.NumEOAs && pipelineErr == nil; i++ {
			acc := cfg.AutoFill.DrawEOA(rng)
			for _, dup := genesisAddrs[acc.Address]; dup; {
				acc = cfg.AutoFill.DrawEOA(rng)
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
		}
		for i := 0; i < cfg.AutoFill.NumContracts && pipelineErr == nil; i++ {
			c := cfg.AutoFill.DrawContract(rng)
			for _, dup := genesisAddrs[c.Address]; dup; {
				c = cfg.AutoFill.DrawContract(rng)
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
		}
	}
	stats.TotalBytes = stats.StorageBytes + stats.CodeBytes

	// Close entityCh → workers drain remaining items then exit.
	close(entityCh)
	encodeWg.Wait()

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
		for i := range cfg.PreAlloc {
			pe := &cfg.PreAlloc[i]
			if pe.Storage == nil {
				continue
			}
			pe.Storage(func(slot, value common.Hash) bool {
				if err := encodeStorageSlot(pipelineCtx, pe.Address, slot, value, chans, &counts); err != nil {
					pipelineErr = err
					return false
				}
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
		{"commitmentInput", commitmentInputStore},
	} {
		if err := fz.store.Finalize(); err != nil {
			return common.Hash{}, fmt.Errorf("writeSnapshots: Finalize %s streamsort: %w", fz.name, err)
		}
	}

	// -- Step 4: HPH commitment walk over commitmentInputStore.
	// ctx.Account/Storage callbacks read from streamsort.Get
	// (disk-backed). branches map stays in memory (bounded).
	result, err := internalcommitment.ComputeGenesisRoot(commitmentInputStore)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: ComputeGenesisRoot: %w", err)
	}
	// KeyCommitmentState encodes (txNum=StepSize-1, blockNum=0): the
	// "fat genesis" anchor. Genesis is made to OCCUPY the entire frozen
	// step 0 by writing MaxTxNum[0]=StepSize-1 (see genesis_patch.go), so
	// the commitment domain's last-committed txNum is StepSize-1 (still
	// step 0, inside the frozen [0,1) file). On boot, SeekCommitment reads
	// this anchor and restoreTxNum seeds the FIRST live block (block 1) at
	// txNum=StepSize — i.e. STEP 1, one step ABOVE the frozen range.
	//
	// This is the load-bearing fix for the block-2 "wrong trie root"
	// stall. The prior (0,0)/(1,0) anchors left block 1 at txNum 2-3
	// (step 0), so its commitment writes to MDBX were SHADOWED by the
	// frozen commitment.0-1.kv via the getLatestFromDb EndTxNum gate
	// (domain.go:1582: an MDBX write at step S wins only when
	// lastTxNumOfStep(S) >= files.EndTxNum()=StepSize, i.e. S>=1). With
	// block 1 at step 1, lastTxNumOfStep(1)=2*StepSize-1 >= StepSize, so
	// the live commitment WINS the gate → not shadowed → continuable.
	// CheckDataAvailable still passes: frozenSteps([0,1))=0, and step 1
	// is not < 0. This mirrors how a snap-synced node resumes with the
	// chain tip past the frozen steps.
	keyStateValue, err := internalcommitment.EncodeKeyCommitmentStateValue(internalerigon.StepSize-1, 0, result.HPHState)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: encode KeyCommitmentState: %w", err)
	}

	// -- Step 5a: marshal the global branches map into a streamsort
	// keyed by branch prefix (sorted for deterministic .kv output).
	branchesStore, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: open branches streamsort: %w", err)
	}
	defer branchesStore.Close()
	var nBranches uint64
	for prefix, data := range result.BranchNodes {
		if err := branchesStore.Put([]byte(prefix), data); err != nil {
			return common.Hash{}, fmt.Errorf("writeSnapshots: put branch %x: %w", []byte(prefix), err)
		}
		nBranches++
	}

	// -- Step 5b: snap.NewWriter + parallel multi-range emit.
	settings := snap.Settings{
		Seed:              cfg.Seed,
		StepSize:          internalerigon.StepSize,
		StepsInFrozenFile: internalerigon.StepsInFrozenFile,
		SnapshotVersion:   internalerigon.SnapshotFormatVersion,
	}
	w, err := snap.NewWriter(cfg.DBPath, settings)
	if err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: snap.NewWriter: %w", err)
	}
	defer w.Close()

	// Fan out 3 independent WriteDomain calls (accounts/storage/code,
	// each at the single [0,1) fullRange). Each call has its own .kv +
	// accessors output, its own streamsort input, and no shared mutable
	// state — safe to run in parallel. Semaphore-bound at NumCPU keeps
	// seg.Compressor pressure realistic on small hosts.
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
	emitErrCh := make(chan error, len(domainSpecs))
	var emitWg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, ds := range domainSpecs {
		sem <- struct{}{}
		emitWg.Add(1)
		go func(ds domainSpec) {
			defer func() { <-sem; emitWg.Done() }()
			if err := w.WriteDomain(ctx, ds.domain, fullRange, ds.count,
				snap.FromStreamsort(ds.store)); err != nil {
				select {
				case emitErrCh <- fmt.Errorf("WriteDomain(%v): %w", ds.domain, err):
				default:
				}
			}
		}(ds)
	}
	emitWg.Wait()
	select {
	case err := <-emitErrCh:
		return common.Hash{}, fmt.Errorf("writeSnapshots: %w", err)
	default:
	}

	// Commitment: single [0,1) range. frozenSteps(commitment)=0 so the
	// daemon's mem-tier writes of KeyCommitmentState at txNum=blockTxNum
	// (stored as step=0) pass CheckDataAvailable trivially (0 < 0 is
	// false). See the fullRange doc above for the full rationale.
	if err := snap.WriteCommitment(ctx, w, fullRange, keyStateValue, branchesStore, nBranches); err != nil {
		return common.Hash{}, fmt.Errorf("writeSnapshots: WriteCommitment: %w", err)
	}

	if cfg.Verbose {
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
	in <-chan domainWrite,
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
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ew, ok := <-in:
			if !ok {
				return nil
			}
			if err := encodeEntity(ctx, ew, out, counts); err != nil {
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
	out *perDomainChans,
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
	if len(ew.entry.Code) > 0 {
		h := crypto.Keccak256Hash(ew.entry.Code)
		copy(acct.CodeHash[:], h[:])
	}
	if err := sendDomainWrite(ctx, out.accounts, domainWrite{key: addrKey, value: account.SerialiseV3(acct)}); err != nil {
		return err
	}
	atomic.AddUint64(&counts.accounts, 1)

	// Code snapshot value: raw bytecode keyed by addr.
	if len(ew.entry.Code) > 0 {
		if err := sendDomainWrite(ctx, out.code, domainWrite{key: addrKey, value: ew.entry.Code}); err != nil {
			return err
		}
		atomic.AddUint64(&counts.code, 1)
	}

	// Inline storage (foundational PreAlloc + contract autofill).
	for slot, value := range ew.entry.Storage {
		if err := encodeStorageSlot(ctx, ew.addr, slot, value, out, counts); err != nil {
			return err
		}
	}

	// Commitment input: account-level Update keyed by plain addr.
	commitBytes := internalcommitment.EncodeAccountUpdate(ew.entry.Nonce, balance, ew.entry.Code)
	return sendDomainWrite(ctx, out.commitIn, domainWrite{key: addrKey, value: commitBytes})
}

// encodeStorageSlot encodes one (addr, slot, value) tuple. Skip on
// all-zero value (Erigon's StorageDomain treats absent ≡ zero;
// storing zero is wrong).
func encodeStorageSlot(
	ctx context.Context,
	addr common.Address,
	slotKey common.Hash,
	slotValue common.Hash,
	out *perDomainChans,
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
	if err := sendDomainWrite(ctx, out.storage, domainWrite{key: plainKey, value: trimmed}); err != nil {
		return err
	}
	atomic.AddUint64(&counts.storage, 1)

	commitBytes := internalcommitment.EncodeStorageUpdate(slotValue[:])
	return sendDomainWrite(ctx, out.commitIn, domainWrite{key: plainKey, value: commitBytes})
}

// sendDomainWrite is the cancel-aware channel send used by encoders.
func sendDomainWrite(ctx context.Context, ch chan<- domainWrite, dw domainWrite) error {
	select {
	case ch <- dw:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// domainWriter drains a single per-domain channel into the
// corresponding streamsort. streamsort is single-writer by design
// (internal/streamsort/streamsort.go) — having exactly one writer
// goroutine per domain is what makes the worker pool safe without a
// mutex on the streamsort itself.
func domainWriter(
	ctx context.Context,
	in <-chan domainWrite,
	store *streamsort.Store,
	label string,
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case dw, ok := <-in:
			if !ok {
				return nil
			}
			if err := store.Put(dw.key, dw.value); err != nil {
				return fmt.Errorf("domainWriter[%s]: %w", label, err)
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
