//go:build cgo_ethrex

package ethrex

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ethereum/state-actor/internal/memstat"
)

// memSampleInterval is how often the writer reports its memory split.
//
// Chosen against the failure it exists to diagnose: a 350 GB fill runs for
// hours and dies without warning, so the last line before the kill has to be
// recent enough to be the cause rather than ancient history. At this cadence a
// two-hour run adds ~240 lines — noise a CI log absorbs, and the only record
// that survives a SIGKILL, which by definition cannot be caught or logged.
const memSampleInterval = 30 * time.Second

// startMemorySampler logs the process memory split every memSampleInterval
// until the returned stop function is called.
//
// The two halves of each report answer different questions. memstat gives RSS
// (what the OOM killer measures) against the Go runtime's own total, and their
// difference is memory no Go-side knob governs. db.memoryReport then attributes
// that difference to RocksDB's own accounting. Anything left over after both is
// neither Go nor RocksDB — Pebble arenas, or something unaccounted.
//
// The caller MUST invoke stop before closing db: the sampler reads DB
// properties, and grocksdb offers no guard against a closed handle. Deferring
// stop after the db.Close defer gives the required LIFO ordering.
func startMemorySampler(ctx context.Context, db *ethrexDB) func() {
	sampleCtx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		ticker := time.NewTicker(memSampleInterval)
		defer ticker.Stop()

		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				log.Printf("  ethrex: mem %s · %s", memstat.Read(), db.memoryReport())
			}
		}
	}()

	return func() {
		cancel()
		wg.Wait()
	}
}
