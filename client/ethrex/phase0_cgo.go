//go:build cgo_ethrex

package ethrex

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
	"github.com/ethereum/state-actor/internal/streamingtrie"
)

// maxPhase0Workers caps Phase 0 drain-and-compute parallelism. Each worker
// holds two per-worker batchSinks (storage_trie_nodes + storage_flatkeyvalue)
// backed by independent grocksdb.WriteBatches. Peak per-worker RAM scales
// with the batchSink flush threshold plus streamingtrie's on-disk sort.
const maxPhase0Workers = 8

// phase0Result carries one entity's computed storage root back to the main
// goroutine so it can update preAllocStorageRoots and GenesisAccounts.
type phase0Result struct {
	addr       common.Address
	addrHash   common.Hash
	root       common.Hash
	localSlots int
	localBytes uint64
}

// runPhase0Storage drains each PreAlloc entity's storage iterator in parallel.
// Each worker owns its own pair of batchSinks (storage_trie_nodes and
// storage_flatkeyvalue) — the per-entity addrHash prefix makes concurrent
// grocksdb.Write calls safe because every worker writes to a disjoint
// keyspace. Storage roots are content-addressed (keccak), so worker completion
// order does not affect the final state root.
//
// Only the calling goroutine touches preAllocStorageRoots, cfg.GenesisAccounts,
// and stats.
func runPhase0Storage(
	ctx context.Context,
	cfg generator.Config,
	db *ethrexDB,
	preAllocStorageRoots map[common.Hash]common.Hash,
	stats *generator.Stats,
) error {
	// Collect indices of PreAlloc entries that have storage to drain.
	indices := make([]int, 0, len(cfg.PreAlloc))
	for i := range cfg.PreAlloc {
		if cfg.PreAlloc[i].Storage != nil {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return nil
	}

	// Phase 0 streams each spec entity's storage slots and is otherwise silent
	// for hours on bloat specs. The count-only slot heartbeat funnels every
	// worker's per-slot count through one SlotMeter; each worker holds its own
	// SlotWorker so the hot per-slot path stays cheap (one non-atomic add + mask).
	cfg.Progress.Stage("ethrex: phase 0 — spec storage")
	slotMeter := cfg.Progress.SlotMeter()

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > maxPhase0Workers {
		workers = maxPhase0Workers
	}
	if workers > len(indices) {
		workers = len(indices)
	}

	drainCtx, cancelDrain := context.WithCancelCause(ctx)
	defer cancelDrain(nil)

	// Each index is sent to drainCh exactly once, so a given pe.Storage iterator
	// (including map-backed closures that iterate a shared map without internal
	// synchronization) is consumed by at most one worker at a time. Do not
	// duplicate indices here.
	drainCh := make(chan int, workers*2)
	resultCh := make(chan phase0Result, workers*4)

	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slotW := slotMeter.Worker()

			// Each worker owns its own write batches targeting the two storage CFs.
			// Per-worker batches avoid contention and keep each worker's writes
			// isolated to its assigned addrHash-prefixed keyspace.
			workerTrieSink := newBatchSink(db, cfIdxStorageTrieNodes)
			workerFkvSink := newBatchSink(db, cfIdxStorageFlatKeyValue)
			defer func() {
				// Close both sinks, but report only the first failure: cancelDrain
				// keeps the first cause, so capture it explicitly rather than letting
				// the second (no-op) call drop a genuine second error silently.
				var closeErr error
				if err := workerTrieSink.Close(); err != nil {
					closeErr = fmt.Errorf("ethrex phase 0: worker trie sink close: %w", err)
				}
				if err := workerFkvSink.Close(); err != nil && closeErr == nil {
					closeErr = fmt.Errorf("ethrex phase 0: worker fkv sink close: %w", err)
				}
				if closeErr != nil {
					cancelDrain(closeErr)
				}
			}()

			// workerRouteSink applies the snap-sync split: leaf full-path rows go
			// to the flat-KV CF; all other rows go to the trie-node CF.
			workerRouteSink := ethrexinternal.NodeSink(func(path []byte, value []byte) error {
				if isLeafFullPathHelper(path) {
					return workerFkvSink.put(path, value)
				}
				return workerTrieSink.put(path, value)
			})

			for i := range drainCh {
				if drainCtx.Err() != nil {
					return
				}
				pe := &cfg.PreAlloc[i]
				addr := pe.Address
				addrHash := crypto.Keccak256Hash(addr[:])

				// Storage rows arrive address-prefixed (via PrefixedSink), so the
				// leaf full-path key is byte-identical to ethrex's own flat-KV key.
				// SuppressEmptyTrieSentinel prevents a spurious (prefix, 0x80) row
				// when the entity's iterator yields zero non-zero slots.
				prefixedSink := ethrexinternal.PrefixedSink(addrHash, workerRouteSink)
				hb := ethrexinternal.NewStreamHashBuilder(ethrexinternal.SuppressEmptyTrieSentinel(prefixedSink))

				// Local counters accumulate stats for this entity; the main goroutine
				// merges them into shared stats after receiving the result.
				var localSlots int
				var localBytes uint64
				statSink := func(_, _, value common.Hash) error {
					slotW.Slot()
					enc := ethrexinternal.EncodeStorageValue(new(uint256.Int).SetBytes32(value[:]))
					localSlots++
					localBytes += uint64(len(enc))
					return nil
				}

				root, err := streamingtrie.StorageRoot(cfg.DBPath, pe.Storage, hb, statSink)
				if err != nil {
					cancelDrain(fmt.Errorf("ethrex: storage root (PreAlloc %s): %w", addr.Hex(), err))
					return
				}

				select {
				case resultCh <- phase0Result{
					addr:       addr,
					addrHash:   addrHash,
					root:       root,
					localSlots: localSlots,
					localBytes: localBytes,
				}:
				case <-drainCtx.Done():
					return
				}
			}
		}()
	}

	// Feed entity indices to workers.
	go func() {
		defer close(drainCh)
		for _, i := range indices {
			select {
			case drainCh <- i:
			case <-drainCtx.Done():
				return
			}
		}
	}()

	// Close resultCh once all workers have finished.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Main goroutine: collect results and update shared state single-threadedly.
	// Determinism is preserved because each root is content-addressed (keccak)
	// and the maps are keyed by addrHash; insertion order does not matter.
	for entry := range resultCh {
		preAllocStorageRoots[entry.addrHash] = entry.root
		if acc, ok := cfg.GenesisAccounts[entry.addr]; ok && acc != nil {
			acc.Root = entry.root
		}
		stats.StorageSlotsCreated += entry.localSlots
		stats.StorageBytes += entry.localBytes
	}

	if cause := context.Cause(drainCtx); cause != nil && cause != context.Canceled {
		return cause
	}
	return nil
}
