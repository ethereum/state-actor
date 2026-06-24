package progress

import "sync/atomic"

// slotStride bounds how often a hot per-slot counter samples the wall clock
// and touches the shared total: once per 2^12 slots. Phase-0 spec-storage
// drains run several parallel workers over up to ~1B slots each; a shared
// atomic incremented on every slot would bounce a cache line across cores
// billions of times and measurably slow the dominant bench phase. Batching the
// shared update into per-worker strides keeps the per-slot cost to a single
// non-atomic increment plus a mask test, and touches the shared atomic only
// once per stride. Must be a power of two for the mask test in Slot.
const slotStride = 1 << 12

// Compile-time guard that slotStride is a power of two (the `&(slotStride-1)`
// mask in Slot means "every Nth call" only then). For a power of two
// `slotStride & (slotStride-1)` is 0, so this is uint(0); otherwise it is a
// negative constant that fails to convert to uint — a build error.
const _ = uint(0 - (slotStride & (slotStride - 1)))

// SlotMeter aggregates a hot, parallel per-slot count into one shared total and
// emits a throttled count-only heartbeat through a Reporter. Construct one per
// phase with NewSlotMeter, hand each worker goroutine its own *SlotWorker via
// Worker, and call Slot once per item. A nil *SlotMeter (built from a nil
// Reporter) is a no-op, so library/test callers stay silent and pay nothing.
//
// The published total is a stride-rounded LOWER BOUND: each worker's final
// sub-stride remainder (< slotStride) is never flushed, so the heartbeat may
// undershoot the true processed count by up to workers×(slotStride-1). That is
// intentional for a count-only line (no percentage/ETA is derived from it) —
// do not feed this total into a percentage and trust it as exact.
type SlotMeter struct {
	r     *Reporter
	total atomic.Int64
}

// NewSlotMeter returns a SlotMeter, or nil when r is nil (the no-op fast path
// for library/test callers that don't wire progress).
func NewSlotMeter(r *Reporter) *SlotMeter {
	if r == nil {
		return nil
	}
	return &SlotMeter{r: r}
}

// SlotMeter is the method form of NewSlotMeter so callers can write
// cfg.Progress.SlotMeter() without importing this package directly (and
// without naming the *SlotMeter type). Safe on a nil *Reporter.
func (r *Reporter) SlotMeter() *SlotMeter {
	return NewSlotMeter(r)
}

// Worker returns a goroutine-local counter. Each worker goroutine MUST use its
// own: SlotWorker holds unsynchronised local state and is not safe to share.
func (m *SlotMeter) Worker() *SlotWorker {
	if m == nil {
		return nil
	}
	return &SlotWorker{m: m}
}

// SlotWorker counts slots for a single goroutine, flushing the running count to
// the shared SlotMeter (and the wall clock, via Reporter.Tick) only once per
// slotStride. A nil *SlotWorker is a no-op.
type SlotWorker struct {
	m     *SlotMeter
	local int64
}

// Slot records one processed slot. The per-call cost is a single non-atomic
// increment and a mask test; the shared atomic total and the Reporter are
// touched only once per stride, keeping cross-core contention negligible — so
// this is cheap enough to call on the hottest per-storage-slot path across
// parallel workers.
func (w *SlotWorker) Slot() {
	if w == nil {
		return
	}
	w.local++
	if w.local&(slotStride-1) == 0 {
		w.m.r.Tick(w.m.total.Add(slotStride), 0, "slots")
	}
}
