package memstat

import (
	"runtime"
	"testing"
)

// TestParseVmRSS pins the kB→bytes conversion and the tolerated shapes of
// /proc/self/status, whose field alignment varies between kernels.
func TestParseVmRSS(t *testing.T) {
	status := "Name:\tstate-actor\nVmPeak:\t 8000000 kB\nVmRSS:\t 4194304 kB\nThreads:\t42\n"

	tests := []struct {
		name   string
		raw    string
		want   uint64
		wantOK bool
	}{
		{name: "typical status file", raw: status, want: 4194304 << 10, wantOK: true},
		{name: "first line", raw: "VmRSS: 1024 kB\n", want: 1 << 20, wantOK: true},
		{name: "extra spacing", raw: "VmRSS:      2048 kB\n", want: 2 << 20, wantOK: true},
		{name: "missing", raw: "Name:\tx\nVmPeak:\t 100 kB\n", want: 0, wantOK: false},
		{name: "empty", raw: "", want: 0, wantOK: false},
		{name: "no value", raw: "VmRSS:\n", want: 0, wantOK: false},
		{name: "not a number", raw: "VmRSS: lots kB\n", want: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseVmRSS([]byte(tt.raw))
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseVmRSS(%q) = (%d, %t), want (%d, %t)",
					tt.raw, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestOffHeap pins the guard against a negative difference. The Go runtime
// counts reserved-but-unfaulted address space that RSS does not, so GoTotal
// legitimately exceeds RSS and must not underflow.
func TestOffHeap(t *testing.T) {
	tests := []struct {
		name   string
		sample Sample
		want   uint64
	}{
		{name: "typical", sample: Sample{RSS: 40 << 30, GoTotal: 12 << 30}, want: 28 << 30},
		{name: "rss unavailable", sample: Sample{RSS: 0, GoTotal: 12 << 30}, want: 0},
		{name: "go total exceeds rss", sample: Sample{RSS: 8 << 30, GoTotal: 12 << 30}, want: 0},
		{name: "equal", sample: Sample{RSS: 8 << 30, GoTotal: 8 << 30}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sample.OffHeap(); got != tt.want {
				t.Errorf("OffHeap() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes uint64
		want  string
	}{
		{bytes: 0, want: "0B"},
		{bytes: 512, want: "512B"},
		{bytes: 1 << 20, want: "1MiB"},
		{bytes: 26 << 30, want: "26.0GiB"},
		{bytes: (3 << 30) + (512 << 20), want: "3.5GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := FormatBytes(tt.bytes); got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

// TestReadReportsGoRuntime checks that the runtime/metrics names still exist:
// a renamed or removed metric silently reads as 0, which would make the whole
// diagnostic useless exactly when it is needed.
func TestReadReportsGoRuntime(t *testing.T) {
	// Hold a live allocation so the object count cannot be legitimately zero.
	ballast := make([]byte, 8<<20)
	defer runtime.KeepAlive(ballast)

	got := Read()
	if got.GoTotal == 0 {
		t.Errorf("Read().GoTotal is 0; metric %q may have been renamed", goTotalMetric)
	}
	if got.GoObjects == 0 {
		t.Errorf("Read().GoObjects is 0; metric %q may have been renamed", goObjectsMetric)
	}
	if got.String() == "" {
		t.Error("Read().String() is empty")
	}
}

// TestParseMemInfo pins extraction of the host-wide fields. Dirty/Writeback
// distinguish "this process ate memory" from "unreclaimable page cache did",
// so a silent parse failure here would hide the more likely cause.
func TestParseMemInfo(t *testing.T) {
	meminfo := "MemTotal:       64909644 kB\n" +
		"MemFree:         1203620 kB\n" +
		"MemAvailable:    2097152 kB\n" +
		"Dirty:           4194304 kB\n" +
		"Writeback:       1048576 kB\n"

	got := parseMemInfo([]byte(meminfo), "MemAvailable", "Dirty", "Writeback")

	want := map[string]uint64{
		"MemAvailable": 2097152 << 10,
		"Dirty":        4194304 << 10,
		"Writeback":    1048576 << 10,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("parseMemInfo()[%q] = %d, want %d", name, got[name], w)
		}
	}

	// MemFree was present but not requested; it must not leak into the result.
	if _, present := got["MemFree"]; present {
		t.Error("parseMemInfo() returned an unrequested field")
	}
}

// TestParseMemInfoTolerates checks that malformed or absent fields degrade to
// a missing key (reading as 0) rather than a bogus value.
func TestParseMemInfoTolerates(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "field absent", raw: "MemTotal: 100 kB\n"},
		{name: "no value", raw: "Dirty:\n"},
		{name: "not a number", raw: "Dirty: lots kB\n"},
		{name: "no colon", raw: "Dirty 100 kB\n"},
		{name: "wraps the kB shift", raw: "Dirty: 18446744073709551615 kB\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMemInfo([]byte(tt.raw), "Dirty")
			if v, present := got["Dirty"]; present {
				t.Errorf("parseMemInfo(%q)[\"Dirty\"] = %d, want it absent", tt.raw, v)
			}
		})
	}
}
