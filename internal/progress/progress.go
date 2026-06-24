// Package progress emits throttled, human-readable heartbeat lines for the
// long-running state-generation phases. A multi-hour run otherwise prints
// nothing between the startup banner and the final summary, which reads as
// "frozen". The Reporter is deliberately tiny: every client funnels its
// generation loop through Tick, and a nil *Reporter is a valid no-op so
// library/test callers that don't wire one stay silent.
package progress

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInterval is the minimum gap between two emitted progress lines for a
// single stage. The hot loops call Tick on every iteration; the throttle keeps
// output to a readable trickle regardless.
const DefaultInterval = 15 * time.Second

// Reporter emits throttled progress for the current stage. The zero value is
// not usable; construct one with New. A nil *Reporter is a valid no-op.
type Reporter struct {
	interval time.Duration

	// lastNano is the wall-clock nanos of the last emitted line. Read/written
	// atomically so Tick's throttle check stays off the mutex on the hot path.
	lastNano atomic.Int64

	mu      sync.Mutex // guards stage/stageAt and serialises log output
	stage   string
	stageAt time.Time
}

// New returns a Reporter that emits at most one line per DefaultInterval.
func New() *Reporter {
	return &Reporter{interval: DefaultInterval}
}

// Stage announces a new phase, resets the rate/ETA baseline, and prints
// immediately so the user gets instant feedback at every phase boundary.
func (r *Reporter) Stage(name string) {
	if r == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.stage = name
	r.stageAt = now
	r.mu.Unlock()
	r.lastNano.Store(now.UnixNano())
	log.Printf("▸ %s", name)
}

// Tick reports progress within the current stage. current/total are item
// counts; pass total <= 0 when the total is unknown (a count-only line with no
// percentage or ETA). detail is an optional suffix, e.g. "1.2M slots". Tick is
// safe to call on every loop iteration: emission is throttled to one line per
// interval and the throttle check is lock-free.
func (r *Reporter) Tick(current, total int64, detail string) {
	if r == nil {
		return
	}
	now := time.Now()
	last := r.lastNano.Load()
	if now.UnixNano()-last < int64(r.interval) {
		return
	}
	// CAS so that, under concurrent ticks, exactly one caller emits per window.
	if !r.lastNano.CompareAndSwap(last, now.UnixNano()) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	elapsed := now.Sub(r.stageAt).Seconds()
	var rate float64
	if elapsed > 0 {
		rate = float64(current) / elapsed
	}

	suffix := ""
	if detail != "" {
		suffix = " · " + detail
	}

	if total > 0 {
		pct := float64(current) / float64(total) * 100
		eta := "—"
		if rate > 0 && current <= total {
			remain := time.Duration(float64(total-current)/rate) * time.Second
			eta = remain.Round(time.Second).String()
		}
		log.Printf("  %s %s/%s (%.1f%%) · %s/s · ETA %s%s",
			r.stage, humanInt(current), humanInt(total), pct, humanRate(rate), eta, suffix)
		return
	}
	log.Printf("  %s %s · %s/s%s", r.stage, humanInt(current), humanRate(rate), suffix)
}

// humanInt formats n with thousands separators, e.g. 1234567 -> "1,234,567".
func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	// Insert a comma every three digits from the right.
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	out = append(out, s[:lead]...)
	for i := lead; i < len(s); i += 3 {
		out = append(out, ',')
		out = append(out, s[i:i+3]...)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// humanRate formats an items-per-second rate compactly, e.g. 12345 -> "12.3k".
func humanRate(perSec float64) string {
	switch {
	case perSec >= 1_000_000:
		return fmt.Sprintf("%.1fM", perSec/1_000_000)
	case perSec >= 1_000:
		return fmt.Sprintf("%.1fk", perSec/1_000)
	default:
		return fmt.Sprintf("%.0f", perSec)
	}
}
