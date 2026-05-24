//go:build cgo_besu && oracle

package besu

import (
	"context"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nerolation/state-actor/generator"
	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/autofill"
	e2e "github.com/nerolation/state-actor/internal/e2e_testing"
	"github.com/nerolation/state-actor/internal/engineapi"
	"github.com/nerolation/state-actor/internal/oracle"
	"github.com/nerolation/state-actor/internal/rpcprobe"
	"github.com/nerolation/state-actor/internal/syscontracts"
)

// pinnedBesuImage is the upstream Besu Docker tag the e2e suite pins
// against. Override with BESU_IMAGE=hyperledger/besu:vX.Y.Z to test a
// pre-release version. Matches the default in
// client/besu/testdata/validate-big-db-besu.sh so smoke + e2e exercise
// the same release.
const pinnedBesuImage = "hyperledger/besu:25.11.0"

func besuImageRef() string {
	if v := os.Getenv("BESU_IMAGE"); v != "" {
		return v
	}
	return pinnedBesuImage
}

// TestE2ESuite — per-PR end-to-end gate for the besu writer + boot path.
// Phase 1-2 (datadir + Run + boot besu) is besu-specific; Phase 3-7
// (capture root → re-query → spamoor → re-query → write result) is in
// internal/e2e.RunSuitePhases — same shape across all 4 client suites.
//
// Build-tagged `cgo_besu && oracle`. Run via `make test-besu-suite`;
// not in plain `go test ./...`. Spamoor binary must be on PATH or
// $SPAMOOR set; absent → t.Skip (or t.Fatal in CI when REQUIRE_SPAMOOR=1).
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

	// All 4 clients pin --fork=osaka after the writer migration to internal/genesisheader.Build. state-actor's
	// MaxForkForClient(c)=="osaka" for every client; pre-Prague forks
	// are EOL and rejected at parse. 60M gas tracks current mainnet.
	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "BESU")
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
		TrieMode:   generator.TrieModeMPT,
		TargetSize: e2eBudget,
		Genesis:    g,
	}
	// Spec-driven pre-alloc via examples/full-matrix-spec-feature.yaml exercises
	// every schema variant (including the spamoor sender). The Spec
	// document is also passed to RunSuitePhases so Phase 4 verifies every
	// erc20 template field via eth_call.
	specDoc, preAlloc := e2e.LoadCISpec(t, "../../examples/full-matrix-spec-feature.yaml", "besu")
	cfg.PreAlloc = preAlloc
	// Deploy EIP-4788/7002/7251/2935 system contracts at their canonical
	// addresses — besu refuses to boot a Prague-active chain without
	// them ("Withdrawal Request Contract Address not found"), and on
	// each block "Invalid system call address" if their code is missing.
	syscontracts.AddCanonicalSystemContracts(&cfg)
	if _, err := Run(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:     seed,
		AutoFill: plan,
	})

	// Boot upstream besu. --genesis-state-hash-cache-enabled tells besu
	// to trust the stored stateRoot rather than recompute it from
	// chainspec.alloc (state-actor writes synthetic state directly into
	// RocksDB). --host-allowlist=all (literal "all" — using "*" would
	// glob-expand against /opt/besu's contents in besu's entrypoint
	// script).
	//
	// Engine API is enabled (port 8551) so the EngineDriver goroutine
	// below can drive block production via engine_forkchoiceUpdated /
	// getPayload / newPayload — besu has no native post-Merge dev mode
	// (clique broken post-Shanghai per hyperledger/besu#8532, removed
	// in 26.4.0). JWT is disabled for tests.
	imageRef := besuImageRef()
	containerName := "state-actor-besu-boot-" + e2e.RandSuffix(8)
	chainspecPath := dd.ContainerDatadir + "/" + ChainSpecFileName
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("BESU_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"--data-path", dd.ContainerDatadir,
		"--genesis-file", chainspecPath,
		"--network-id", "1337",
		"--data-storage-format", "BONSAI",
		"--genesis-state-hash-cache-enabled",
		"--rpc-http-enabled",
		"--rpc-http-host", "0.0.0.0",
		"--rpc-http-port", "8545",
		"--rpc-http-api", "ETH,NET,WEB3",
		"--host-allowlist", "all",
		"--min-gas-price", "0",
		"--engine-rpc-enabled",
		"--engine-rpc-port", "8551",
		// Literal "all" — passing "*" would glob-expand against
		// /opt/besu's contents in besu's entrypoint script (same bug
		// the --host-allowlist comment above documents).
		"--engine-host-allowlist", "all",
		"--engine-jwt-disabled",
		"--logging", "INFO",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("besu container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("besu container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()    //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nbesu logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("besu JSON-RPC: %s", rpcURL)

	// Besu's JVM startup is slower than geth's Go startup — give it 180s.
	if err := rpcprobe.WaitForRPC(rpcURL, 180*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup): %v", err)
	}

	// Mock CL: drive block production via engine API. Modeled after besu's
	// own PragueAcceptanceTestHelper.java. The goroutine runs for the
	// rest of the test so spamoor's txs (Phase 5) get included in blocks.
	e2e.StartEngineDriver(t, containerIP, rpcURL, engineapi.ForkOsaka)

	// Phases 3-7: shared via internal/e2e.RunSuitePhases.
	e2e.RunSuitePhases(t, e2e.SuitePhasesCfg{
		ClientName:      "besu",
		RPCURL:          rpcURL,
		EOAs:            eoas,
		Contracts:       contracts,
		GeneratorConfig: &cfg,
		Spec:            specDoc,
		SpecSeed:        e2e.CISpecSeed,
	})
}
