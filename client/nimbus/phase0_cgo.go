//go:build cgo_nimbus

package nimbus

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/ethrex"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
	"github.com/ethereum/state-actor/internal/streamingtrie"
)

// maxPhase0Workers caps Phase 0 parallelism; each worker owns one AriVtx
// batchSink and a streamingtrie on-disk sort.
const maxPhase0Workers = 8

type phase0Result struct {
	addr       common.Address
	addrHash   common.Hash
	pre        preStorage
	localSlots int
	localBytes uint64
}

// runPhase0Storage streams every spec entity's storage into its own storage
// trie. stoIDs are allocated up front in addrHash order so the assignment is
// deterministic regardless of worker timing; each trie's rows go to the
// worker's own sink under root stoID, a keyspace disjoint from every other
// trie. Results land in preStorage (keyed by addrHash) for Stage A.
func runPhase0Storage(
	ctx context.Context,
	cfg generator.Config,
	db *nimbusDB,
	alloc *nimbusinternal.VidAllocator,
	pre map[common.Hash]preStorage,
	stats *generator.Stats,
) error {
	type job struct {
		index    int
		addrHash common.Hash
		stoID    nimbusinternal.VertexID
	}
	jobs := make([]job, 0, len(cfg.PreAlloc))
	for i := range cfg.PreAlloc {
		if cfg.PreAlloc[i].Storage != nil {
			jobs = append(jobs, job{index: i, addrHash: crypto.Keccak256Hash(cfg.PreAlloc[i].Address[:])})
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	sort.Slice(jobs, func(i, j int) bool { return bytes.Compare(jobs[i].addrHash[:], jobs[j].addrHash[:]) < 0 })
	for i := range jobs {
		jobs[i].stoID = alloc.Alloc(1)
	}

	cfg.Progress.Stage("nimbus: phase 0 — spec storage")
	slotMeter := cfg.Progress.SlotMeter()

	workers := runtime.NumCPU()
	if workers > maxPhase0Workers {
		workers = maxPhase0Workers
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers < 1 {
		workers = 1
	}

	drainCtx, cancelDrain := context.WithCancelCause(ctx)
	defer cancelDrain(nil)

	jobCh := make(chan job, workers*2)
	resultCh := make(chan phase0Result, workers*4)

	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slotW := slotMeter.Worker()
			sink := newBatchSinkWithThreshold(db, cfIdxAriVtx, workerFlushThresholdBytes)
			defer func() {
				if err := sink.Close(); err != nil {
					cancelDrain(fmt.Errorf("nimbus phase 0: worker sink close: %w", err))
				}
			}()
			vsink := sink.vertexSink()
			for j := range jobCh {
				if drainCtx.Err() != nil {
					return
				}
				pe := &cfg.PreAlloc[j.index]
				hb := nimbusinternal.NewStreamHashBuilder(j.stoID, alloc, vsink)
				var localSlots int
				var localBytes uint64
				statSink := func(_, _, value common.Hash) error {
					slotW.Slot()
					localSlots++
					localBytes += uint64(ethrex.StorageValueRLPLength(value))
					return nil
				}
				root, err := streamingtrie.StorageRoot(cfg.DBPath, pe.Storage, hb, statSink)
				if err != nil {
					cancelDrain(fmt.Errorf("nimbus: storage root (PreAlloc %s): %w", pe.Address.Hex(), err))
					return
				}
				res := phase0Result{addr: pe.Address, addrHash: j.addrHash, localSlots: localSlots, localBytes: localBytes,
					pre: preStorage{root: root, stoID: j.stoID, stoHint: hb.StoHint()}}
				if hb.LeafCount() == 0 {
					// Every slot was zero: no storage trie, the reserved ID stays an
					// unused gap below vTop (harmless — nimbus never reads it).
					res.pre = preStorage{root: nimbusinternal.EmptyRootHash}
				}
				select {
				case resultCh <- res:
				case <-drainCtx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-drainCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	for r := range resultCh {
		pre[r.addrHash] = r.pre
		if acc, ok := cfg.GenesisAccounts[r.addr]; ok && acc != nil {
			acc.Root = r.pre.root
		}
		stats.StorageSlotsCreated += r.localSlots
		stats.StorageBytes += r.localBytes
	}
	if cause := context.Cause(drainCtx); cause != nil && cause != context.Canceled {
		return cause
	}
	return nil
}
