package progress

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNilSlotMeterIsNoOp(t *testing.T) {
	out := captureLog(t, func() {
		m := NewSlotMeter(nil) // nil Reporter → nil meter
		require.Nil(t, m)
		w := m.Worker() // nil meter → nil worker
		require.Nil(t, w)
		for range slotStride * 2 {
			w.Slot() // must not panic
		}
	})
	assert.Empty(t, out)
}

func TestSlotMeterBatchesToSharedTotal(t *testing.T) {
	r := New()
	r.interval = time.Hour // suppress emission; we only check the total
	m := NewSlotMeter(r)
	w := m.Worker()

	// One short of a stride: nothing flushed to the shared total yet.
	for range slotStride - 1 {
		w.Slot()
	}
	assert.Equal(t, int64(0), m.total.Load(), "shared total must not move before a full stride")

	w.Slot() // crosses the stride boundary
	assert.Equal(t, int64(slotStride), m.total.Load())

	for range slotStride {
		w.Slot()
	}
	assert.Equal(t, int64(2*slotStride), m.total.Load())
}

func TestSlotMeterEmitsCountOnly(t *testing.T) {
	r := New()
	r.interval = time.Millisecond
	out := captureLog(t, func() {
		r.Stage("phase 0")
		// Push the baseline into the past so the stride flush is eligible to emit.
		r.lastNano.Store(time.Now().Add(-time.Second).UnixNano())
		w := m0Worker(r)
		for range slotStride {
			w.Slot()
		}
	})
	assert.Contains(t, out, "slots")
	assert.NotContains(t, out, "ETA") // count-only: no percentage / ETA
	assert.NotContains(t, out, "%")
}

// m0Worker is a tiny helper so the test reads at the call site.
func m0Worker(r *Reporter) *SlotWorker { return NewSlotMeter(r).Worker() }

// TestSlotMeterConcurrent is the -race guard: many workers hammering their own
// SlotWorkers must funnel into the shared total without data races, and the
// total must equal the exact number of full strides completed.
func TestSlotMeterConcurrent(t *testing.T) {
	r := New()
	r.interval = time.Hour
	m := NewSlotMeter(r)

	const workers = 8
	const perWorker = slotStride * 4

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := m.Worker()
			for range perWorker {
				w.Slot()
			}
		}()
	}
	wg.Wait()

	// Each worker completes exactly perWorker/slotStride strides; no leftover.
	want := int64(workers * perWorker)
	assert.Equal(t, want, m.total.Load())
}

// guard against an accidentally non-power-of-two stride (the mask test relies
// on it).
func TestSlotStrideIsPowerOfTwo(t *testing.T) {
	assert.Equal(t, 0, slotStride&(slotStride-1), "slotStride must be a power of two")
}
