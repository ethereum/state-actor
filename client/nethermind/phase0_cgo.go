//go:build cgo_neth

package nethermind

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/generator"
	nethtrie "github.com/ethereum/state-actor/internal/neth/trie"
	"github.com/ethereum/state-actor/internal/streamingtrie"
)

// runPhase0 drains each PreAlloc entity's storage iter in parallel, splicing
// the computed storage root into genesisAccounts. Workers own per-instance
// stateDBSinks + nethtrie.Builders; codeSink is shared via internal mutex.
// addrHash-prefixed keyspaces make concurrent grocksdb.Write safe.
// Determinism: per-entity storage roots are content-addressed (keccak), so
// worker completion order doesn't affect the final state root.
func runPhase0(
	ctx context.Context,
	cfg generator.Config,
	dbs *nethDBs,
	genesisAccounts map[common.Address]*types.StateAccount,
	stats *generator.Stats,
) error {
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
	cfg.Progress.Stage("nethermind: phase 0 — spec storage")
	slotMeter := cfg.Progress.SlotMeter()

	// TODO: long-pole-first scheduling — pe.Storage is iter.Seq2 so len() is
	// unavailable without consuming the iterator.

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

	type phase0Result struct {
		addr         common.Address
		root         common.Hash
		storageBytes uint64
	}

	drainCtx, cancelDrain := context.WithCancelCause(ctx)
	defer cancelDrain(nil)

	drainCh := make(chan int, workers*2)
	resultCh := make(chan phase0Result, workers*4)

	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slotW := slotMeter.Worker()
			workerSink := newStateDBSink(dbs.state)
			defer func() {
				if err := workerSink.close(); err != nil {
					cancelDrain(fmt.Errorf("nethermind phase 0: worker sink close: %w", err))
				}
			}()
			workerBuilder := nethtrie.NewBuilder(workerSink)

			for i := range drainCh {
				if drainCtx.Err() != nil {
					return
				}
				pe := &cfg.PreAlloc[i]
				addr := pe.Address
				addrHash := crypto.Keccak256Hash(addr[:])
				ah := [32]byte(addrHash)
				hb := &nethermindStorageHashBuilder{builder: workerBuilder, ah: ah}

				var entityStorageBytes uint64
				statSink := func(_, _, value common.Hash) error {
					slotW.Slot()
					// RLP-of-trimmed-value byte count (1 prefix + len(trimmed)).
					v := value[:]
					for len(v) > 0 && v[0] == 0 {
						v = v[1:]
					}
					if len(v) > 0 {
						entityStorageBytes += uint64(len(v) + 1)
					}
					return nil
				}
				root, err := streamingtrie.StorageRoot(cfg.DBPath, pe.Storage, hb, statSink)
				if err != nil {
					cancelDrain(fmt.Errorf("nethermind: stream spec storage[%d] %s: %w", i, addr.Hex(), err))
					return
				}
				select {
				case resultCh <- phase0Result{addr: addr, root: root, storageBytes: entityStorageBytes}:
				case <-drainCtx.Done():
					return
				}
			}
		}()
	}

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

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	for entry := range resultCh {
		if acc, ok := genesisAccounts[entry.addr]; ok && acc != nil {
			acc.Root = entry.root
		}
		if stats != nil {
			stats.StorageBytes += entry.storageBytes
		}
	}
	if cause := context.Cause(drainCtx); cause != nil && cause != context.Canceled {
		return cause
	}
	return nil
}
