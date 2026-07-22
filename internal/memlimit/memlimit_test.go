package memlimit

import (
	"math"
	"strings"
	"testing"
)

const gib = uint64(1) << 30

// TestBudget pins the split arithmetic, including the two cases where Budget
// must decline: a reserve that swallows the host, and a remainder too small
// for a limit to help rather than stall the collector.
func TestBudget(t *testing.T) {
	tests := []struct {
		name    string
		total   uint64
		reserve uint64
		want    int64
	}{
		{
			// The host that OOM-killed the ethrex 350 GB run: 61.9 GiB with
			// the writer's 10 GiB off-heap reserve subtracted, halved.
			name:    "benchmark host",
			total:   66467475456,
			reserve: 10 * gib,
			want:    int64((66467475456 - 10*gib) / 2),
		},
		{
			name:    "reserve equals total",
			total:   16 * gib,
			reserve: 16 * gib,
			want:    0,
		},
		{
			name:    "reserve exceeds total",
			total:   8 * gib,
			reserve: 10 * gib,
			want:    0,
		},
		{
			name:    "remainder below the useful floor",
			total:   12 * gib,
			reserve: 10 * gib,
			want:    0,
		},
		{
			name:    "remainder exactly at the useful floor",
			total:   14 * gib,
			reserve: 10 * gib,
			want:    minUsefulLimit,
		},
		{
			name:    "undetected total",
			total:   0,
			reserve: 10 * gib,
			want:    0,
		},
		{
			// Halving MaxUint64 lands exactly on MaxInt64; the conversion in
			// Budget must not wrap negative.
			name:    "maximum total does not overflow int64",
			total:   math.MaxUint64,
			reserve: 0,
			want:    math.MaxInt64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Budget(tt.total, tt.reserve); got != tt.want {
				t.Errorf("Budget(%d, %d) = %d, want %d", tt.total, tt.reserve, got, tt.want)
			}
		})
	}
}

// TestParseCgroupLimit covers both spellings of "unlimited", since treating
// either as a real ceiling would shadow the host's true MemTotal.
func TestParseCgroupLimit(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   uint64
		wantOK bool
	}{
		{name: "v2 byte count", raw: "8589934592\n", want: 8 * gib, wantOK: true},
		{name: "v2 unlimited", raw: "max\n", want: 0, wantOK: false},
		{name: "v1 unlimited sentinel", raw: "9223372036854771712\n", want: 0, wantOK: false},
		{name: "empty", raw: "", want: 0, wantOK: false},
		{name: "whitespace only", raw: "  \n", want: 0, wantOK: false},
		{name: "not a number", raw: "unexpected\n", want: 0, wantOK: false},
		{name: "negative", raw: "-1\n", want: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCgroupLimit([]byte(tt.raw))
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseCgroupLimit(%q) = (%d, %t), want (%d, %t)",
					tt.raw, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestParseMemTotal pins the kB→bytes conversion and the MemTotal line being
// found regardless of position.
func TestParseMemTotal(t *testing.T) {
	// The header of the failing run's host, as /proc/meminfo renders it.
	meminfo := "MemTotal:       64909644 kB\nMemFree:        49203620 kB\nBuffers:          123456 kB\n"

	tests := []struct {
		name   string
		raw    string
		want   uint64
		wantOK bool
	}{
		{name: "first line", raw: meminfo, want: 64909644 << 10, wantOK: true},
		{
			name:   "not the first line",
			raw:    "SwapTotal:             0 kB\nMemTotal:        1048576 kB\n",
			want:   1 * gib,
			wantOK: true,
		},
		{name: "missing", raw: "MemFree: 100 kB\n", want: 0, wantOK: false},
		{name: "empty", raw: "", want: 0, wantOK: false},
		{name: "no value", raw: "MemTotal:\n", want: 0, wantOK: false},
		{name: "not a number", raw: "MemTotal:  lots kB\n", want: 0, wantOK: false},
		{
			name:   "value large enough to wrap the kB shift",
			raw:    "MemTotal: 18446744073709551615 kB\n",
			want:   0,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseMemTotal([]byte(tt.raw))
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseMemTotal(%q) = (%d, %t), want (%d, %t)",
					tt.raw, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestResultString checks that each Result shape renders a line a reader can
// act on — an applied limit names its source, a declined one names its cause.
func TestResultString(t *testing.T) {
	tests := []struct {
		name  string
		res   Result
		wants []string
	}{
		{
			name:  "applied",
			res:   Result{Applied: true, Limit: 26 * int64(gib), Total: 62 * gib, Source: "/proc/meminfo MemTotal"},
			wants: []string{"26.0 GiB", "62.0 GiB", "/proc/meminfo MemTotal"},
		},
		{
			name:  "declined with a reason",
			res:   Result{Reason: "could not determine the host memory ceiling"},
			wants: []string{"no Go memory limit applied", "could not determine"},
		},
		{
			name:  "never called",
			res:   Result{},
			wants: []string{"no Go memory limit applied"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.res.String()
			for _, want := range tt.wants {
				if !strings.Contains(got, want) {
					t.Errorf("Result.String() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// TestSetIsIdempotent pins the sync.Once contract: repeated calls, including
// with a different reserve, return the first decision unchanged.
//
// It deliberately does not assert Applied — the test binary inherits whatever
// GOMEMLIMIT the runner has set, and declining in that case is correct.
func TestSetIsIdempotent(t *testing.T) {
	first := Set(1 * gib)
	second := Set(100 * gib)

	if first != second {
		t.Errorf("Set returned %+v then %+v; want the first decision both times", first, second)
	}
}
