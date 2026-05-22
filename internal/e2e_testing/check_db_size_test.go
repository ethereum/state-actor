package e2e_testing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDBSize_WithinTolerance(t *testing.T) {
	dir := t.TempDir()
	// Write a single 1 MiB file.
	const oneMiB = 1 << 20
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, oneMiB), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pretend expected is 1.05 MiB → 5 % off, within ±10 % tolerance.
	if !CheckDBSize(t, dir, oneMiB+oneMiB/20, 0.10) {
		t.Errorf("expected pass within ±10%% tolerance")
	}
}

func TestCheckDBSize_OutsideTolerance(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pretend expected is 2 MiB → 100 % off, fails ±10 %.
	sub := &capturedT{T: t}
	if CheckDBSize(sub, dir, 2<<20, 0.10) {
		t.Errorf("expected failure when actual is 50%% of expected")
	}
	if !sub.errored {
		t.Errorf("expected t.Errorf to have been called")
	}
}

func TestCheckDBSize_ZeroExpectedSkips(t *testing.T) {
	if !CheckDBSize(t, "/nonexistent/intentionally", 0, 0.10) {
		t.Errorf("expected true (skip) when ExpectedBytes==0")
	}
}

func TestCheckDBSize_WalkError(t *testing.T) {
	sub := &capturedT{T: t}
	if CheckDBSize(sub, "/nonexistent/path/should/not/exist", 1<<20, 0.10) {
		t.Errorf("expected failure on walk error")
	}
	if !sub.errored {
		t.Errorf("expected t.Errorf on walk error")
	}
}

// capturedT wraps *testing.T to capture whether Errorf was called without
// failing the parent test. Implements testing.TB so CheckDBSize accepts it.
type capturedT struct {
	*testing.T
	errored bool
}

func (c *capturedT) Errorf(format string, args ...any) {
	c.errored = true
	c.T.Logf("[captured] "+format, args...)
}

func (c *capturedT) Fatalf(format string, args ...any) {
	c.errored = true
	c.T.Logf("[captured-fatal] "+format, args...)
}
