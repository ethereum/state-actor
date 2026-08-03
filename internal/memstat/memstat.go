// Package memstat reads a point-in-time picture of this process's memory:
// the kernel's resident-set size alongside the Go runtime's own accounting.
//
// The two together are the diagnostic. RSS is what the OOM killer measures,
// and the Go runtime's total is what a GOMEMLIMIT governs. A writer that
// commits most of its memory through cgo — RocksDB block caches and
// memtables, Pebble arenas — shows the gap between them as memory no Go-side
// knob can influence, which is exactly the thing a heap profile cannot see.
//
// Sampling is deliberately cheap enough to run on a progress heartbeat:
// readings come from runtime/metrics rather than runtime.ReadMemStats, so no
// stop-the-world pause is incurred, and RSS is one small /proc read.
package memstat

import (
	"fmt"
	"math"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
)

// Kernel memory files. Both are absent on non-Linux, in which case the
// corresponding Sample fields read as 0.
//
// procMemInfoPath reports HOST-wide figures even from inside a container:
// /proc is not namespaced for meminfo, and no lxcfs is in play here.
const (
	procStatusPath  = "/proc/self/status"
	procMemInfoPath = "/proc/meminfo"
)

// Metric names read from runtime/metrics. goTotalMetric counts every byte the
// Go runtime has mapped from the OS — heap, stacks, and runtime structures —
// which is the quantity GOMEMLIMIT actually bounds. goObjectsMetric is the
// live objects PLUS dead-not-yet-swept ones (per the runtime/metrics
// definition), accumulated at size-class granularity — a sawtooth between
// ≈live and ≈the GC goal (2×live at GOGC=100), NOT a live-set floor.
// Reading it as live cost a debugging cycle once; divide by ~1-2×.
const (
	goTotalMetric   = "/memory/classes/total:bytes"
	goObjectsMetric = "/memory/classes/heap/objects:bytes"
)

// Sample is one reading. All values are bytes; fields the platform does not
// expose read as 0.
type Sample struct {
	// RSS is the process resident-set size as the kernel reports it.
	RSS uint64
	// GoTotal is every byte the Go runtime has mapped from the OS.
	GoTotal uint64
	// GoObjects is /memory/classes/heap/objects: live objects plus dead
	// objects not yet swept — see the goObjectsMetric comment; not a live
	// floor.
	GoObjects uint64
	// HostAvailable is MemAvailable: memory the kernel believes it can hand
	// out without swapping, host-wide.
	//
	// This is deliberately the HOST's figure, not the container's. /proc is
	// not namespaced for meminfo, so a container without lxcfs reads through
	// to the host — which is what we want. It distinguishes "this process
	// exhausted memory" from "this process was modest and something else, or
	// unreclaimable page cache, exhausted the host". Those two have opposite
	// fixes, and an exit code of 137 alone cannot tell them apart.
	HostAvailable uint64
	// HostDirty is dirty page cache awaiting writeback, host-wide. Page cache
	// is normally reclaimable and therefore harmless, but dirty pages are not
	// reclaimable until written back: a writer that outruns its storage can
	// pin gigabytes here and drive the host to OOM while its own RSS stays
	// flat.
	HostDirty uint64
	// HostWriteback is page cache actively being written back, host-wide.
	HostWriteback uint64
}

// OffHeap returns the resident bytes not accounted for by the Go runtime —
// cgo allocations (RocksDB, Pebble) plus mapped binary and stacks. It is the
// term a Go memory limit cannot govern. Returns 0 when RSS is unavailable or
// when the Go runtime's own total exceeds RSS, which happens legitimately
// because Go counts reserved-but-unfaulted address space that RSS does not.
func (s Sample) OffHeap() uint64 {
	if s.RSS == 0 || s.GoTotal > s.RSS {
		return 0
	}

	return s.RSS - s.GoTotal
}

// String renders the sample as one log line.
func (s Sample) String() string {
	return fmt.Sprintf(
		"rss=%s go-total=%s go-objects=%s off-heap=%s host-avail=%s host-dirty=%s host-writeback=%s",
		FormatBytes(s.RSS), FormatBytes(s.GoTotal),
		FormatBytes(s.GoObjects), FormatBytes(s.OffHeap()),
		FormatBytes(s.HostAvailable), FormatBytes(s.HostDirty),
		FormatBytes(s.HostWriteback))
}

// Read takes a sample. It never fails: values it cannot obtain read as 0.
func Read() Sample {
	samples := []metrics.Sample{
		{Name: goTotalMetric},
		{Name: goObjectsMetric},
	}
	metrics.Read(samples)

	s := Sample{RSS: readRSS()}
	// A metric the runtime does not recognise comes back as KindBad; treat it
	// as absent rather than reading a garbage union field.
	if samples[0].Value.Kind() == metrics.KindUint64 {
		s.GoTotal = samples[0].Value.Uint64()
	}
	if samples[1].Value.Kind() == metrics.KindUint64 {
		s.GoObjects = samples[1].Value.Uint64()
	}

	if raw, err := os.ReadFile(procMemInfoPath); err == nil {
		fields := parseMemInfo(raw, "MemAvailable", "Dirty", "Writeback")
		s.HostAvailable = fields["MemAvailable"]
		s.HostDirty = fields["Dirty"]
		s.HostWriteback = fields["Writeback"]
	}

	return s
}

// readRSS returns the process resident-set size in bytes, or 0 when the
// platform does not expose /proc/self/status.
func readRSS() uint64 {
	raw, err := os.ReadFile(procStatusPath)
	if err != nil {
		return 0
	}

	value, ok := parseVmRSS(raw)
	if !ok {
		return 0
	}

	return value
}

// parseVmRSS extracts VmRSS from /proc/self/status content, converting the
// kernel's kB to bytes.
func parseVmRSS(raw []byte) (uint64, bool) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		rest, found := strings.CutPrefix(line, "VmRSS:")
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

		return value << 10, true
	}

	return 0, false
}

// parseMemInfo extracts the named fields from /proc/meminfo content, keyed by
// name without the trailing colon. Values are converted from the file's kB to
// bytes. Fields that are absent or malformed are simply missing from the
// result, so a caller reading a missing key gets 0.
func parseMemInfo(raw []byte, names ...string) map[string]uint64 {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}

	out := make(map[string]uint64, len(names))
	for line := range strings.SplitSeq(string(raw), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if _, want := wanted[name]; !want {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}

		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || value > math.MaxUint64>>10 {
			continue
		}

		out[name] = value << 10
	}

	return out
}

// FormatBytes renders a byte count with a binary unit suffix, at a precision
// that keeps memory log lines scannable.
func FormatBytes(bytes uint64) string {
	switch {
	case bytes >= 1<<30:
		return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64) + "GiB"
	case bytes >= 1<<20:
		return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 0, 64) + "MiB"
	default:
		return strconv.FormatUint(bytes, 10) + "B"
	}
}
