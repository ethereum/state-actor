// Package memlimit derives and applies a Go soft memory limit
// (runtime/debug.SetMemoryLimit) for writers whose peak RSS is a mix of Go
// heap and off-heap cgo allocations.
//
// The problem: with GOGC=100 and no limit, the Go heap grows to roughly twice
// the live set before a collection. A writer that pins several GiB of spec
// bytecode for a whole run therefore reserves several GiB more it never
// needs — on top of a RocksDB/Pebble footprint the Go runtime cannot see,
// because block caches and memtable arenas are C malloc. On a memory-capped
// host the OOM killer measures the sum.
//
// Set reads the host's real ceiling (cgroup v2, then cgroup v1, then
// /proc/meminfo), subtracts the caller's declared off-heap reserve, and hands
// the Go heap a heapDivisor share of the remainder (a third today — see the
// heapDivisor comment for why not half). The rest absorbs the soft limit's
// transient overshoot and, above all, the reserve being an underestimate.
//
// A limit below the live heap is worse than no limit: the collector then runs
// continuously (Go caps it at 50% of CPU) and the process crawls instead of
// failing fast. Set declines to apply anything below minUsefulLimit and
// reports why, rather than guessing.
//
// Set never overrides a limit that is already in effect, so an explicit
// GOMEMLIMIT in the environment always wins.
package memlimit
