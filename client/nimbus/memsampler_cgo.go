//go:build cgo_nimbus

package nimbus

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ethereum/state-actor/internal/memstat"
)

// memSampleInterval is how often the writer logs its memory split — the only
// record that survives an OOM kill (see the ethrex writer's rationale).
const memSampleInterval = 30 * time.Second

// startMemorySampler logs RSS vs Go-heap vs RocksDB accounting every
// memSampleInterval until the returned stop function is called. The caller
// MUST stop it before db.Close(): it reads DB properties and grocksdb does
// not guard against a closed handle.
func startMemorySampler(ctx context.Context, db *nimbusDB) func() {
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
				log.Printf("  nimbus: mem %s · %s", memstat.Read(), db.memoryReport())
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}
