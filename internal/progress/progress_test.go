package progress

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanInt(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, humanInt(c.in), "humanInt(%d)", c.in)
	}
}

func TestHumanRate(t *testing.T) {
	assert.Equal(t, "0", humanRate(0))
	assert.Equal(t, "999", humanRate(999))
	assert.Equal(t, "1.0k", humanRate(1000))
	assert.Equal(t, "12.3k", humanRate(12345))
	assert.Equal(t, "1.5M", humanRate(1_500_000))
}

// captureLog redirects the stdlib logger to a buffer for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

func TestNilReporterIsNoOp(t *testing.T) {
	var r *Reporter
	out := captureLog(t, func() {
		r.Stage("x")           // must not panic
		r.Tick(1, 10, "items") // must not panic
	})
	assert.Empty(t, out)
}

func TestStagePrintsImmediately(t *testing.T) {
	r := New()
	out := captureLog(t, func() {
		r.Stage("phase 1")
	})
	assert.Contains(t, out, "phase 1")
}

func TestTickThrottlesWithinInterval(t *testing.T) {
	r := New()
	r.interval = time.Hour // nothing should emit after the Stage baseline
	out := captureLog(t, func() {
		r.Stage("phase")
		for i := range 1000 {
			r.Tick(int64(i), 1000, "items")
		}
	})
	// Only the Stage line, no Tick lines.
	assert.Equal(t, 1, strings.Count(out, "\n"))
	assert.NotContains(t, out, "ETA")
}

func TestTickEmitsAfterInterval(t *testing.T) {
	r := New()
	r.interval = time.Millisecond
	out := captureLog(t, func() {
		r.Stage("phase")
		// Force the baseline into the past so the first Tick is eligible.
		r.lastNano.Store(time.Now().Add(-time.Second).UnixNano())
		r.Tick(250, 1000, "items")
	})
	require.Contains(t, out, "phase")
	assert.Contains(t, out, "250/1,000")
	assert.Contains(t, out, "(25.0%)")
	assert.Contains(t, out, "ETA")
	assert.Contains(t, out, "items")
}

func TestTickUnknownTotalOmitsPctAndETA(t *testing.T) {
	r := New()
	r.interval = time.Millisecond
	out := captureLog(t, func() {
		r.Stage("phase")
		r.lastNano.Store(time.Now().Add(-time.Second).UnixNano())
		r.Tick(500, 0, "")
	})
	assert.Contains(t, out, "500")
	assert.NotContains(t, out, "ETA")
	assert.NotContains(t, out, "%")
}
