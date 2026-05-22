package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestMainBenchmarkPrintsStats verifies the --benchmark flag's stdout
// path end-to-end:
//   - the flag is wired (main.go's `if *benchmark { ... }` branch runs)
//   - every writer populates non-zero AccountBytes / StorageBytes /
//     CodeBytes (otherwise the printed stats are silently zero — the
//     class of bug issue #70 was filed for)
//
// Runs against --client=geth (the default; no Docker dependency, in-
// process Populate). 10 EOAs + 3 contracts with at least 1 slot each is
// enough to make all three byte counts strictly positive.
//
// The test invokes `go build` to compile state-actor once into a temp
// binary, then runs that binary with `--benchmark`. Geth is the default
// client and the only one that builds without `cgo_neth`/`cgo_reth` tags
// (Docker-only writers), so the geth path is what's exercised here. The
// nethermind/reth equivalents are pinned at writer-unit level in
// client/{nethermind,reth}/run_cgo_stats_test.go and
// client/reth/{data,contracts}_writer_cgo_test.go.
func TestMainBenchmarkPrintsStats(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark e2e in short mode")
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "state-actor")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build state-actor: %v\n%s", err, out)
	}

	dbDir := filepath.Join(binDir, "chaindata")
	run := exec.Command(binPath,
		"--db", dbDir,
		"--target-size", "2MB",
		"--seed", "42",
		"--benchmark",
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("state-actor --benchmark exited: %v\n%s", err, out)
	}
	stdout := string(out)

	// Section header must appear (proves the `if *benchmark { ... }` branch fired).
	if !strings.Contains(stdout, "=== Detailed Stats ===") {
		t.Fatalf("--benchmark output missing '=== Detailed Stats ===' section header.\nFull stdout:\n%s", stdout)
	}

	// Per-category byte counts must be strictly positive — a writer that
	// doesn't populate stats.{Account,Storage,Code}Bytes would print 0.
	for _, category := range []string{"Account Bytes", "Storage Bytes", "Code Bytes"} {
		if got := parseByteRow(t, stdout, category); got == 0 {
			t.Errorf("--benchmark reports %q == 0 (writer didn't populate the corresponding Stats field). Full stdout:\n%s", category, stdout)
		}
	}
}

// parseByteRow extracts the numeric byte count from a "Category: N.NN MB"
// row in state-actor's stdout. Returns 0 if the row is absent OR the value
// is "0 B". formatBytes (main.go) emits one of the SI scales so this matches
// "N B", "N KB", "N MB", "N GB", "N TB" — any non-empty number scales to
// non-zero, and "0 B" returns 0.
func parseByteRow(t *testing.T, stdout, category string) uint64 {
	t.Helper()
	// e.g. "Account Bytes:     1.23 KB" or "Account Bytes:     0 B"
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(category) + `:\s+([0-9.]+)\s+([KMGT]?B)$`)
	m := re.FindStringSubmatch(stdout)
	if m == nil {
		t.Errorf("could not find %q row in stdout", category)
		return 0
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Errorf("parse %q value %q: %v", category, m[1], err)
		return 0
	}
	// Any non-zero numeric is fine — we just need to detect zero.
	mult := uint64(1)
	switch m[2] {
	case "B":
		mult = 1
	case "KB":
		mult = 1024
	case "MB":
		mult = 1024 * 1024
	case "GB":
		mult = 1024 * 1024 * 1024
	case "TB":
		mult = 1024 * 1024 * 1024 * 1024
	}
	return uint64(val * float64(mult))
}

// TestMainSpecFlagSmoke verifies the --spec end-to-end happy path: a YAML
// spec file loads, parses, validates, expands, and the writer produces a
// non-empty datadir without erroring. Runs against --client=geth (the
// only no-Docker writer) so it lives in the default CI job.
//
// This is the per-feature companion to TestMainBenchmarkPrintsStats — it
// pins the wiring from CLI flag → spec parser → templates registry →
// specbuild translator → Config.PreAlloc → writer. A regression in any
// of those layers fails this test.
func TestMainSpecFlagSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping --spec smoke test in short mode")
	}

	// Build state-actor.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "state-actor")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Write a minimal spec file inline — exercises raw contract + 7702
	// EOA + plain EOA, all three address resolution modes.
	specPath := filepath.Join(binDir, "spec.yaml")
	const specYAML = `entities:
  - kind: contract
    name: my-contract
    code: "0x6080"
  - kind: eoa
    address: 0x1111111111111111111111111111111111111111
    balance: "1000000000000000000"
  - kind: eoa
    name: delegator
    code: "0xef0100aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	dbDir := filepath.Join(binDir, "chaindata")
	cmd := exec.Command(binPath,
		"--db", dbDir,
		"--spec", specPath,
		"--seed", "42",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("state-actor --spec exited: %v\n%s", err, out)
	}

	// Validate the writer actually produced files. We don't pin a specific
	// subdir layout (geth's path discipline can evolve); just that the db
	// directory exists and is non-empty after the run.
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("read db dir: %v\n--- state-actor stdout ---\n%s", err, out)
	}
	if len(entries) == 0 {
		t.Errorf("db dir empty after --spec run; spec entities were not persisted\n--- state-actor stdout ---\n%s", out)
	}
}

// TestMainInjectAccountsFlagRemoved confirms the --inject-accounts flag
// is not accepted. Users who pass it should hit Go's "flag provided but
// not defined" error rather than the flag being silently ignored.
// Pinpoints the absence at CLI level so a future PR can't accidentally
// add it back.
func TestMainInjectAccountsFlagRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping --inject-accounts removal test in short mode")
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "state-actor")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(binPath, "--inject-accounts=0x1111111111111111111111111111111111111111", "--db=/tmp/never")
	out, _ := cmd.CombinedOutput()
	// Exit code must be non-zero AND stderr must mention the unknown flag.
	if cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("expected non-zero exit when passing removed --inject-accounts flag")
	}
	if !strings.Contains(string(out), "inject-accounts") {
		t.Errorf("expected error to mention 'inject-accounts'; got:\n%s", out)
	}
}

// TestMainRemovedSyntheticFillFlags pins the absence of every flag that
// the autofill rewrite (#82 follow-up) removed. Users who still pass any
// of the 6 must hit Go's "flag provided but not defined" error.
func TestMainRemovedSyntheticFillFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping removed-flag regression in short mode")
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "state-actor")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cases := []struct {
		flag string
		val  string
	}{
		{"accounts", "10"},
		{"contracts", "5"},
		{"max-slots", "100"},
		{"min-slots", "1"},
		{"distribution", "power-law"},
		{"code-size", "256"},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			cmd := exec.Command(binPath, "--"+tc.flag+"="+tc.val, "--db=/tmp/never")
			out, _ := cmd.CombinedOutput()
			if cmd.ProcessState.ExitCode() == 0 {
				t.Errorf("expected non-zero exit when passing removed --%s flag", tc.flag)
			}
			if !strings.Contains(string(out), tc.flag) {
				t.Errorf("expected error to mention %q; got:\n%s", tc.flag, out)
			}
		})
	}
}

// TestMainRequiresTargetSizeOrSpec confirms that running state-actor with
// neither --spec nor --target-size errors out cleanly.
func TestMainRequiresTargetSizeOrSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping no-spec-no-target regression in short mode")
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "state-actor")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(binPath, "--db="+filepath.Join(binDir, "db"))
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("expected non-zero exit when --spec and --target-size are both absent")
	}
	if !strings.Contains(string(out), "target-size") {
		t.Errorf("expected error to mention 'target-size'; got:\n%s", out)
	}
}
