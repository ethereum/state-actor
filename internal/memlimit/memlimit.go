package memlimit

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// Memory-ceiling sources, in the order detect consults them. The smallest
// real value wins: a container under cgroup v2 sees both its own memory.max
// and the host's MemTotal, and only the former binds it.
//
// Known limitation: the cgroup paths are root-relative. Under a private
// cgroup namespace (docker/podman — every deployment this writer targets)
// that IS the calling container's own limit. A nested cgroup withOUT a
// namespace (e.g. `systemd-run -p MemoryMax=`) keeps its limit at a subtree
// path that would need a /proc/self/cgroup walk to find; detection then
// falls back to MemTotal, which errs toward a higher (less protective)
// ceiling. Deliberately not handled until a real run path needs it.
const (
	cgroupV2MaxPath = "/sys/fs/cgroup/memory.max"
	cgroupV1MaxPath = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	procMemInfoPath = "/proc/meminfo"
)

const (
	// heapDivisor splits the post-reserve memory between the Go heap and the
	// margin absorbing what a soft limit cannot govern.
	//
	// Half, now that the reserve is measured rather than guessed. The
	// interim divisor-3 was chosen when the caller's reserve carried a
	// 12 GiB guess for unmeasured allocator behavior — "the margin's real
	// job is absorbing the reserve being wrong". The 40 GB A/B that gated
	// this constant measured the jemalloc off-heap plateau (~4.4 GiB)
	// UNDER the budgeted caps, so the reserve now over-covers observation
	// and the margin returns to its designed job: soft-limit overshoot
	// plus residual estimate error. TestBudgetLeavesRoomForTheReserve pins
	// the paired invariant (limit+reserve ≤ 3/4 of the host — see there).
	//
	// Erring high costs GC cycles. Erring low costs the whole run, hours in.
	heapDivisor = 2

	// minUsefulLimit is the floor below which applying a limit does more
	// harm than not applying one. See the package doc: a limit under the
	// live heap converts an OOM kill into an unbounded GC stall, which is
	// strictly harder to diagnose. Below this, Set reports and declines.
	minUsefulLimit = 2 << 30

	// unlimitedSentinel is the threshold above which a cgroup limit means
	// "no limit". cgroup v2 writes the literal "max"; cgroup v1 writes a
	// near-uint64-max page count scaled to bytes, whose exact value varies
	// with page size, so compare against a threshold rather than a constant.
	unlimitedSentinel = 1 << 62
)

// Result reports what Set decided. A zero Applied with an empty Reason means
// Set was never called.
type Result struct {
	// Applied is true when this call installed the limit.
	Applied bool
	// Limit is the Go soft memory limit in bytes. When Applied is false but
	// a limit was already in effect, it carries that pre-existing value.
	Limit int64
	// Total is the detected memory ceiling in bytes; 0 when detection failed.
	Total uint64
	// Source names where Total came from.
	Source string
	// Reason explains why no limit was applied. Empty when Applied.
	Reason string
}

// String renders a one-line summary suitable for a startup log.
func (r Result) String() string {
	if r.Applied {
		return fmt.Sprintf("Go memory limit set to %s (%s reports %s)",
			formatGiB(uint64(r.Limit)), r.Source, formatGiB(r.Total))
	}
	if r.Reason == "" {
		return "no Go memory limit applied"
	}
	return "no Go memory limit applied: " + r.Reason
}

var (
	setOnce   sync.Once
	setResult Result
)

// Set installs a Go soft memory limit derived from the host's memory ceiling
// minus reserve, the caller's expected off-heap (cgo) footprint in bytes.
//
// It is idempotent: the first call decides, and every later call returns that
// same Result. Callers should log the Result — a declined limit is a signal
// worth surfacing, not a silent no-op.
func Set(reserve uint64) Result {
	setOnce.Do(func() { setResult = apply(reserve) })

	return setResult
}

// apply performs the one-time decision behind Set.
func apply(reserve uint64) Result {
	// SetMemoryLimit(-1) reads the current limit without changing it. Anything
	// other than the default means GOMEMLIMIT was set in the environment or an
	// earlier caller already chose — either way, defer to it.
	if current := debug.SetMemoryLimit(-1); current != math.MaxInt64 {
		return Result{
			Limit:  current,
			Reason: fmt.Sprintf("a limit of %s is already in effect", formatGiB(uint64(current))),
		}
	}

	total, source, ok := detect()
	if !ok {
		return Result{Reason: "could not determine the host memory ceiling"}
	}

	limit := Budget(total, reserve)
	if limit == 0 {
		return Result{
			Total:  total,
			Source: source,
			Reason: fmt.Sprintf(
				"%s reports %s and %s is reserved off-heap, leaving less than the %s floor",
				source, formatGiB(total), formatGiB(reserve), formatGiB(minUsefulLimit)),
		}
	}

	debug.SetMemoryLimit(limit)

	return Result{Applied: true, Limit: limit, Total: total, Source: source}
}

// Budget computes the Go soft memory limit for a host with total bytes of
// usable memory and reserve bytes committed outside the Go heap. It returns 0
// when no useful limit can be derived — see minUsefulLimit.
//
// The subtraction cannot overflow int64: (total-reserve) is at most MaxUint64,
// and dividing by heapDivisor (>= 2) keeps the result at or under MaxInt64.
func Budget(total, reserve uint64) int64 {
	if total == 0 || reserve >= total {
		return 0
	}

	limit := (total - reserve) / heapDivisor
	if limit < minUsefulLimit {
		return 0
	}

	return int64(limit)
}

// detect returns the smallest real memory ceiling among the known sources,
// along with the name of the source that supplied it.
func detect() (uint64, string, bool) {
	var (
		best   uint64
		source string
	)

	consider := func(value uint64, ok bool, name string) {
		if !ok || value == 0 {
			return
		}
		if best == 0 || value < best {
			best, source = value, name
		}
	}

	if raw, err := os.ReadFile(cgroupV2MaxPath); err == nil {
		value, ok := parseCgroupLimit(raw)
		consider(value, ok, "cgroup v2 memory.max")
	}
	if raw, err := os.ReadFile(cgroupV1MaxPath); err == nil {
		value, ok := parseCgroupLimit(raw)
		consider(value, ok, "cgroup v1 memory.limit_in_bytes")
	}
	if raw, err := os.ReadFile(procMemInfoPath); err == nil {
		value, ok := parseMemTotal(raw)
		consider(value, ok, "/proc/meminfo MemTotal")
	}

	return best, source, best > 0
}

// parseCgroupLimit reads a cgroup memory-limit file. It reports ok=false for
// the two spellings of "unlimited" (v2's literal "max", v1's near-uint64-max
// sentinel) so an unbounded cgroup does not shadow a real ceiling.
func parseCgroupLimit(raw []byte) (uint64, bool) {
	field := strings.TrimSpace(string(raw))
	if field == "" || field == "max" {
		return 0, false
	}

	value, err := strconv.ParseUint(field, 10, 64)
	if err != nil || value >= unlimitedSentinel {
		return 0, false
	}

	return value, true
}

// parseMemTotal extracts MemTotal from /proc/meminfo content. The value is
// reported in kB, which this converts to bytes.
func parseMemTotal(raw []byte) (uint64, bool) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		rest, found := strings.CutPrefix(line, "MemTotal:")
		if !found {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}

		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		// Guard the kB→bytes shift against a value large enough to wrap.
		if value > math.MaxUint64>>10 {
			return 0, false
		}

		return value << 10, true
	}

	return 0, false
}

// formatGiB renders a byte count as GiB with one decimal, for log lines.
func formatGiB(bytes uint64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64) + " GiB"
}
