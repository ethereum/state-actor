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

	"github.com/ethereum/go-ethereum/common"

	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/generator"
	e2e "github.com/nerolation/state-actor/internal/e2e_testing"
	"github.com/nerolation/state-actor/internal/oracle"
	iReth "github.com/nerolation/state-actor/internal/reth"
	"github.com/nerolation/state-actor/internal/rpcprobe"
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

	cfg := generator.Config{DBPath: dd.HostPath}
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
	for _, table := range []string{"PlainAccountState", "HashedAccounts", "Bytecodes"} {
		if !strings.Contains(output, table) {
			t.Errorf("expected table %q in db stats output, got:\n%s", table, output)
		}
	}
}

// TestRethDbStatsSyntheticEOAs — diagnostic: 100-EOA datadir produces
// >=100 entries in the EOA-touched tables.
func TestRethDbStatsSyntheticEOAs(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle test in short mode")
	}
	const numAccounts = 100

	dd, cleanup := e2e.AcquireDatadir(t, "RETH")
	defer cleanup()

	cfg := generator.Config{DBPath: dd.HostPath, NumAccounts: numAccounts, Seed: 12345}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats.AccountsCreated != numAccounts {
		t.Fatalf("AccountsCreated = %d, want %d", stats.AccountsCreated, numAccounts)
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
	checks := map[string]int{
		"PlainAccountState": numAccounts,
		"HashedAccounts":    numAccounts,
		"AccountsHistory":   numAccounts,
		"AccountChangeSets": numAccounts,
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

	cfg := generator.Config{DBPath: dd.HostPath} // empty alloc
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
		"node", "--dev",
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
		exec.Command("docker", "stop", containerName).Run()    //nolint:errcheck
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
//     --debug.skip-genesis-validation; previously the CPerezz/reth fork
//     before the upstream merge on 2026-05-06).
//   - --dev mode auto-mines on tx, so spamoor advances the chain without
//     an external CL.
//
// Build-tagged `cgo_reth && oracle`. Run via `make test-reth-suite`.
func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e suite skipped in short mode")
	}

	const (
		seed         = int64(42)
		numAccounts  = 100
		numContracts = 15_000 // ~100 MB warmup before spamoor (avg 27 slots × 240 B/entry)
		codeSize     = 128
		minSlots     = 5
		maxSlots     = 50
	)

	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "RETH")
	defer cleanup()

	cfg := generator.Config{
		DBPath:          dd.HostPath,
		NumAccounts:     numAccounts,
		NumContracts:    numContracts,
		CodeSize:        codeSize,
		MinSlots:        minSlots,
		MaxSlots:        maxSlots,
		Seed:            seed,
		Verbose:         true,
		Genesis:         g,
		InjectAddresses: []common.Address{oracle.SpamoorSenderAddr},
	}
	// Deploy EIP-4788/2935/7002/7251 system contracts at their canonical
	// addresses — required for the cross-client genesis-root invariant.
	oracle.AddPragueSystemContracts(&cfg)

	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:         seed,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Distribution: cfg.Distribution,
	})

	imageRef := rethImageRef()
	containerName := "state-actor-reth-boot-" + e2e.RandSuffix(8)
	chainspecPath := dd.ContainerDatadir + "/chainspec.json"
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("RETH_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"node", "--dev",
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
		exec.Command("docker", "stop", containerName).Run()    //nolint:errcheck
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
	})
}
