//go:build cgo_reth && oracle

package reth

import (
	"context"
	"math/big"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nerolation/state-actor/generator"
	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/autofill"
	e2e "github.com/nerolation/state-actor/internal/e2e_testing"
	"github.com/nerolation/state-actor/internal/oracle"
	iReth "github.com/nerolation/state-actor/internal/reth"
	"github.com/nerolation/state-actor/internal/rpcprobe"
	"github.com/nerolation/state-actor/internal/syscontracts"
)

// rethImageRef returns the fully-qualified reth image reference from the
// pinned constants, falling back to known-good defaults.
func rethImageRef() string {
	image := iReth.PinnedRethImage
	if image == "" {
		image = "ghcr.io/paradigmxyz/reth"
	}
	tag := iReth.PinnedRethRelease
	if tag == "" {
		tag = "nightly"
	}
	return image + ":" + tag
}

// parseDbStatsEntries extracts the numeric entry count for table from the
// output of `reth db stats`. Returns (count, ok). The output format uses pipe
// separators; the table name appears in column 1 and the entry count in
// column 2. Conservative: returns false if the format doesn't match.
func parseDbStatsEntries(output, table string) (int, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, table) {
			continue
		}
		fields := strings.Fields(strings.ReplaceAll(line, "|", " "))
		for i, f := range fields {
			if f == table && i+1 < len(fields) {
				if n, err := strconv.Atoi(strings.TrimSpace(fields[i+1])); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

// TestRethDbStats — diagnostic: empty-alloc datadir + `reth db stats`.
// Verifies state-actor's MDBX layout (page size, table set, schema
// version) is structurally valid. Cheap and orthogonal to TestE2ESuite.
func TestRethDbStats(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle test in short mode")
	}

	dd, cleanup := e2e.AcquireDatadir(t, "RETH")
	defer cleanup()

	cfg := generator.Config{DBPath: dd.HostPath, AutoFill: rethTestPlan(t, 512<<10), Seed: 12345}
	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	args := append([]string{"run", "--rm"}, e2e.DockerPlatformArgs("RETH_DOCKER_PLATFORM")...)
	args = append(args,
		"-v", dd.VolMount,
		rethImageRef(),
		"db", "--datadir", dd.ContainerDatadir, "stats",
	)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("reth db stats failed:\noutput:\n%s\nerr: %v", out, err)
	}
	output := string(out)
	// HashedAccounts + Bytecodes are the v2-canonical existence signals.
	for _, table := range []string{"HashedAccounts", "Bytecodes"} {
		if !strings.Contains(output, table) {
			t.Errorf("expected table %q in db stats output, got:\n%s", table, output)
		}
	}
}

// TestRethDbStatsSyntheticEOAs — diagnostic: small auto-fill datadir produces
// at least plan.NumEOAs entries in the EOA-touched tables.
func TestRethDbStatsSyntheticEOAs(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle test in short mode")
	}

	dd, cleanup := e2e.AcquireDatadir(t, "RETH")
	defer cleanup()

	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{DBPath: dd.HostPath, AutoFill: plan, Seed: 12345, Archive: true}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats.AccountsCreated != plan.NumEOAs {
		t.Fatalf("AccountsCreated = %d, want %d", stats.AccountsCreated, plan.NumEOAs)
	}

	args := append([]string{"run", "--rm"}, e2e.DockerPlatformArgs("RETH_DOCKER_PLATFORM")...)
	args = append(args,
		"-v", dd.VolMount,
		rethImageRef(),
		"db", "--datadir", dd.ContainerDatadir, "stats",
	)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("reth db stats failed:\noutput:\n%s\nerr: %v", out, err)
	}
	output := string(out)
	// Under v2: PlainAccountState is empty (drop from checks). AccountsHistory
	// has moved to a RocksDB column family which `reth db stats` does NOT
	// enumerate as an MDBX table — also drop. HashedAccounts (canonical v2
	// state) and AccountChangeSets (still MDBX in archive mode) remain.
	checks := map[string]int{
		"HashedAccounts":    plan.NumEOAs,
		"AccountChangeSets": plan.NumEOAs,
	}
	for table, minEntries := range checks {
		count, ok := parseDbStatsEntries(output, table)
		if !ok {
			t.Errorf("could not parse entry count for table %q from output:\n%s", table, output)
			continue
		}
		if count < minEntries {
			t.Errorf("table %q: %d entries, want >= %d", table, count, minEntries)
		}
	}
}

// TestRethNodeBootEmptyAlloc — diagnostic: verify reth boots against an
// empty-alloc datadir (state-actor produces a valid genesis header even
// when no accounts are written). Lighter than TestE2ESuite — useful when
// triaging whether a boot failure is in header encoding or in account/
// storage state.
func TestRethNodeBootEmptyAlloc(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle boot test skipped in short mode")
	}

	dd, cleanup := e2e.AcquireDatadir(t, "RETH")
	defer cleanup()

	cfg := generator.Config{DBPath: dd.HostPath, AutoFill: rethTestPlan(t, 512<<10), Seed: 12345}
	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	imageRef := rethImageRef()
	containerName := "state-actor-reth-boot-empty-" + e2e.RandSuffix(8)
	chainspecPath := dd.ContainerDatadir + "/chainspec.json"
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("RETH_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"node", "--dev", "--dev.block-time", "1s",
		"--chain", chainspecPath,
		"--datadir", dd.ContainerDatadir,
		"--debug.skip-genesis-validation",
		"--http",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("reth container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("reth container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()     //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nreth logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	if err := rpcprobe.WaitForRPC(rpcURL, 120*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs in t.Cleanup): %v", err)
	}
	t.Log("empty-alloc reth node booted successfully")
}

// TestE2ESuite — see client/besu/e2e_test.go for the full phase
// description. reth-specific bits:
//   - --fork=osaka (reth's MaxForkForClient ceiling, after the writer migration to internal/genesisheader.Build).
//   - 60M gas matches mainnet-current.
//   - DinD via the `oracle` build tag.
//   - Pinned image: paradigmxyz/reth digest-pinned via
//     internal/reth.PinnedRethRelease (post-#23919, supports
//     --debug.skip-genesis-validation).
//   - --dev mode auto-mines on tx, so spamoor advances the chain without
//     an external CL.
//
// Build-tagged `cgo_reth && oracle`. Run via `make test-reth-suite`.
func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e suite skipped in short mode")
	}

	const (
		seed = int64(42)
		// e2eBudget targets ~100 MB of synthetic trie state (matching the
		// pre-rewrite NumContracts=15_000 warmup).
		e2eBudget uint64 = 100 << 20
	)

	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "RETH")
	defer cleanup()

	plan, err := autofill.PlanForBudget(e2eBudget)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{
		DBPath:     dd.HostPath,
		AutoFill:   plan,
		Seed:       seed,
		Verbose:    true,
		TargetSize: e2eBudget,
		Genesis:    g,
	}
	// Spec-driven pre-alloc via examples/full-matrix-spec-feature.yaml
	// exercises every schema variant (including the spamoor sender). The
	// Spec is also passed to RunSuitePhases for the Phase 4 erc20 oracle.
	specDoc, preAlloc := e2e.LoadCISpec(t, "../../examples/full-matrix-spec-feature.yaml", "reth")
	cfg.PreAlloc = preAlloc
	// Deploy EIP-4788/2935/7002/7251 system contracts at their canonical
	// addresses — required for the cross-client genesis-root invariant.
	syscontracts.AddCanonicalSystemContracts(&cfg)

	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:     seed,
		AutoFill: plan,
	})

	imageRef := rethImageRef()
	containerName := "state-actor-reth-boot-" + e2e.RandSuffix(8)
	chainspecPath := dd.ContainerDatadir + "/chainspec.json"
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("RETH_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"node", "--dev", "--dev.block-time", "1s",
		"--chain", chainspecPath,
		"--datadir", dd.ContainerDatadir,
		"--debug.skip-genesis-validation",
		"--http",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth,net,web3,txpool",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("reth container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("reth container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()     //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nreth logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("reth JSON-RPC: %s", rpcURL)

	if err := rpcprobe.WaitForRPC(rpcURL, 120*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup): %v", err)
	}

	// Phases 3-7: shared via internal/e2e.RunSuitePhases.
	e2e.RunSuitePhases(t, e2e.SuitePhasesCfg{
		ClientName:      "reth",
		RPCURL:          rpcURL,
		EOAs:            eoas,
		Contracts:       contracts,
		GeneratorConfig: &cfg,
		Spec:            specDoc,
		SpecSeed:        e2e.CISpecSeed,
	})
}
