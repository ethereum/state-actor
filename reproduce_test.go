package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildStateActor compiles the CLI once into a temp dir and returns the path.
// Mirrors the go-build + exec.Command harness used across main_test.go (geth is
// the default, no-cgo writer, so this runs in the default CI job).
func buildStateActor(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "state-actor")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build state-actor: %v\n%s", err, out)
	}
	return bin
}

// extractField returns the value after a "Label:   value" row in stdout.
func extractField(t *testing.T, stdout, label string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, label) {
			return strings.TrimSpace(strings.TrimPrefix(line, label))
		}
	}
	t.Fatalf("could not find %q row in output:\n%s", label, stdout)
	return ""
}

// generateGeth runs a small geth generation and returns (manifestPath, stateRoot).
func generateGeth(t *testing.T, bin, dbDir string, extraArgs ...string) (string, string) {
	t.Helper()
	args := append([]string{"--db", dbDir, "--target-size", "2MB", "--seed", "42"}, extraArgs...)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("generate exited: %v\n%s", err, out)
	}
	s := string(out)
	return extractField(t, s, "Manifest:"), extractField(t, s, "State Root:")
}

// TestReproduceRoundTripMatchesStateRoot is the core contract of the feature: a
// run reproduced into a fresh --db lands on the recorded state root.
func TestReproduceRoundTripMatchesStateRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reproduce round-trip e2e in short mode")
	}
	bin := buildStateActor(t)
	manifestPath, origRoot := generateGeth(t, bin, filepath.Join(t.TempDir(), "orig", "chaindata"))

	reproDB := filepath.Join(t.TempDir(), "repro", "chaindata")
	out, err := exec.Command(bin, "reproduce", "--manifest", manifestPath, "--db", reproDB).CombinedOutput()
	if err != nil {
		t.Fatalf("reproduce exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Reproduction: PASS") {
		t.Errorf("expected 'Reproduction: PASS' (root %s); got:\n%s", origRoot, out)
	}
	// Reproducing with the identical binary must NOT emit the version-mismatch
	// warning (the manifest stores the resolved version; reproduce compares
	// against the same resolved value).
	if strings.Contains(string(out), "reproduction may differ") {
		t.Errorf("unexpected spurious version-mismatch warning on identical binary:\n%s", out)
	}
	// The reproduced datadir gets its own manifest linking back to the source
	// via reproduced_from (reproDB's parent isn't "geth", so DatadirRoot is
	// reproDB itself).
	reproManifest, err := os.ReadFile(filepath.Join(reproDB, "state-actor-manifest.json"))
	if err != nil {
		t.Fatalf("read reproduced manifest: %v", err)
	}
	if !strings.Contains(string(reproManifest), `"reproduced_from"`) ||
		!strings.Contains(string(reproManifest), manifestPath) {
		t.Errorf("reproduced manifest missing reproduced_from=%q:\n%s", manifestPath, reproManifest)
	}
}

// TestReproduceWallClockSeedRoundTrip pins the headline property: a --seed=0 run
// (non-reproducible inputs) reproduces exactly, because the manifest captured the
// resolved concrete seed.
func TestReproduceWallClockSeedRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping --seed=0 reproduce e2e in short mode")
	}
	bin := buildStateActor(t)
	// generateGeth passes --seed 42; override with a trailing --seed 0 (last wins).
	manifestPath, _ := generateGeth(t, bin, filepath.Join(t.TempDir(), "orig", "chaindata"), "--seed", "0")

	reproDB := filepath.Join(t.TempDir(), "repro", "chaindata")
	out, err := exec.Command(bin, "reproduce", "--manifest", manifestPath, "--db", reproDB).CombinedOutput()
	if err != nil {
		t.Fatalf("reproduce of --seed=0 run exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Reproduction: PASS") {
		t.Errorf("expected PASS reproducing a --seed=0 run; got:\n%s", out)
	}
}

// TestReproduceMismatchExitsNonZero verifies the safety contract: when the
// regenerated root differs from the recorded one, reproduce exits non-zero.
func TestReproduceMismatchExitsNonZero(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reproduce mismatch e2e in short mode")
	}
	bin := buildStateActor(t)
	manifestPath, origRoot := generateGeth(t, bin, filepath.Join(t.TempDir(), "orig", "chaindata"))

	// Tamper the recorded state root so regeneration cannot match it.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	bogus := "0x" + strings.Repeat("0", 64)
	if bogus == origRoot {
		bogus = "0x" + strings.Repeat("f", 64)
	}
	if err := os.WriteFile(manifestPath, []byte(strings.ReplaceAll(string(data), origRoot, bogus)), 0o644); err != nil {
		t.Fatalf("write tampered manifest: %v", err)
	}

	reproDB := filepath.Join(t.TempDir(), "repro", "chaindata")
	cmd := exec.Command(bin, "reproduce", "--manifest", manifestPath, "--db", reproDB)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("expected non-zero exit on state-root mismatch; got:\n%s", out)
	}
	if !strings.Contains(string(out), "MISMATCH") {
		t.Errorf("expected 'MISMATCH' in output; got:\n%s", out)
	}
}

// TestReproduceRefusesNonEmptyDB pins the fresh-output-dir guard.
func TestReproduceRefusesNonEmptyDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reproduce fresh-db guard e2e in short mode")
	}
	bin := buildStateActor(t)
	manifestPath, _ := generateGeth(t, bin, filepath.Join(t.TempDir(), "orig", "chaindata"))

	// A pre-existing, non-empty output directory must be refused.
	busyDB := filepath.Join(t.TempDir(), "busy")
	if err := os.MkdirAll(busyDB, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(busyDB, "preexisting"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(bin, "reproduce", "--manifest", manifestPath, "--db", busyDB)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("expected non-zero exit for non-empty --db; got:\n%s", out)
	}
	if !strings.Contains(string(out), "fresh") {
		t.Errorf("expected a 'fresh directory' message; got:\n%s", out)
	}
}

// TestReproduceRefusesOriginalDatadir verifies reproduce won't clobber the
// manifest's own source datadir.
func TestReproduceRefusesOriginalDatadir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reproduce original-datadir guard e2e in short mode")
	}
	bin := buildStateActor(t)
	origDB := filepath.Join(t.TempDir(), "orig", "chaindata")
	manifestPath, _ := generateGeth(t, bin, origDB)

	cmd := exec.Command(bin, "reproduce", "--manifest", manifestPath, "--db", origDB)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("expected non-zero exit when reproducing into the original datadir; got:\n%s", out)
	}
	if !strings.Contains(string(out), "original datadir") {
		t.Errorf("expected an 'original datadir' message; got:\n%s", out)
	}
}

// TestReproduceRequiresManifestAndDB mirrors the project's CLI-contract tests:
// missing required flags must exit non-zero.
func TestReproduceRequiresManifestAndDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reproduce required-flags regression in short mode")
	}
	bin := buildStateActor(t)
	cases := [][]string{
		{"reproduce"},
		{"reproduce", "--manifest", "/tmp/m.json"},
		{"reproduce", "--db", "/tmp/d"},
		{"reproduce", "--manifest", filepath.Join(t.TempDir(), "nope.json"), "--db", filepath.Join(t.TempDir(), "out")},
	}
	for _, args := range cases {
		cmd := exec.Command(bin, args...)
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() == 0 {
			t.Errorf("expected non-zero exit for `reproduce %v`; got:\n%s", args[1:], out)
		}
	}
}

// TestSamePath unit-tests the original-datadir comparison helper.
func TestSamePath(t *testing.T) {
	rel := "foo"
	relAbs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "/x/y", "/x/y", true},
		{"trailing slash vs none", "/x/y/", "/x/y", true},
		{"dotdot normalizes", "/x/z/../y", "/x/y", true},
		{"relative vs absolute", rel, relAbs, true},
		{"different dirs", "/x/y", "/x/z", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := samePath(c.a, c.b); got != c.want {
				t.Errorf("samePath(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
