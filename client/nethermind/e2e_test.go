//go:build cgo_neth && oracle

package nethermind

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/state-actor/generator"
	stategenesis "github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
	e2e "github.com/ethereum/state-actor/internal/e2e_testing"
	"github.com/ethereum/state-actor/internal/engineapi"
	"github.com/ethereum/state-actor/internal/oracle"
	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// pinnedNethImage is the upstream Nethermind Docker tag the Osaka e2e
// suite pins against. Override with NETH_IMAGE=nethermind/nethermind:vX.Y.Z
// to test a pre-release version.
const pinnedNethImage = "nethermind/nethermind:1.39.0"

// pinnedNethAmsterdamImage is the tag TestE2ESuiteAmsterdam pins against.
// 2.1.0 is the Nethermind release for the Sepolia Amsterdam activation
// (2026-10-06). Its Chains/sepolia.json sets the Amsterdam EIP keys at
// 0x6ac4fd60 (1791294816), the go-ethereum v1.17.6 Sepolia AmsterdamTime.
// 1.39.x has no Amsterdam support.
const pinnedNethAmsterdamImage = "nethermind/nethermind:2.1.0"

// nethImageRef returns the image for the fork. NETH_IMAGE overrides both.
func nethImageRef(fork string) string {
	if v := os.Getenv("NETH_IMAGE"); v != "" {
		return v
	}
	if stategenesis.ForkAtLeast(fork, "amsterdam") {
		return pinnedNethAmsterdamImage
	}
	return pinnedNethImage
}

// nethermindE2EConfigTemplate is the Nethermind config for the e2e suite.
// Inlined rather than loaded from testdata so DinD mode (container sees
// /data/<testName>) and direct mode (/data) can both resolve at runtime.
//
// Engine RPC on port 8551 + UnsecureDevNoRpcAuthentication=true is the
// symmetric equivalent of besu's --engine-rpc-enabled / --engine-jwt-disabled.
// Merge.Enabled + TerminalTotalDifficulty=0 + the Ethash chainspec engine
// hand block production to MergePlugin via the engine API from genesis;
// EthashPlugin's PoW path never runs.
//
// Two %s placeholders, in order:
//  1. ChainSpecPath  — full path to state-actor's parity-chainspec.json
//  2. BaseDbPath     — full path to the per-test datadir
const nethermindE2EConfigTemplate = `{
  "Init": {
    "EnableUnsecuredDevWallet": true,
    "KeepDevWalletInMemory": true,
    "DiscoveryEnabled": false,
    "PeerManagerEnabled": false,
    "ChainSpecPath": "%s",
    "BaseDbPath": "%s",
    "MemoryHint": 256000000
  },
  "Sync": {
    "PivotNumber": 0
  },
  "TxPool": {
    "Size": 128,
    "BlobsSupport": "Disabled"
  },
  "Network": {
    "ActivePeersMaxCount": 0
  },
  "JsonRpc": {
    "Enabled": true,
    "Timeout": 20000,
    "Host": "0.0.0.0",
    "Port": 8545,
    "EnabledModules": ["Eth", "Net", "Web3"],
    "EngineHost": "0.0.0.0",
    "EnginePort": 8551,
    "EngineEnabledModules": ["Engine", "Eth", "Net", "Web3", "Subscribe"],
    "UnsecureDevNoRpcAuthentication": true
  },
  "Metrics": {"Enabled": false},
  "Merge": {"Enabled": true, "TerminalTotalDifficulty": "0"},
  "Mining": {"Enabled": false}
}`

// TestE2ESuite — see client/besu/e2e_test.go for the full phase
// description. Phase 1-2 (datadir + Run + boot nethermind) is per-client;
// Phase 3-7 is internal/e2e.RunSuitePhases.
//
// nethermind-specific bits:
//   - --fork=osaka (unified across all 4 clients after the writer migration to internal/genesisheader.Build)
//   - Ethash-from-genesis chainspec + Merge.Enabled → engine-API-driven
//     block production via the EngineDriver mock CL (mirrors besu).
//   - SlotDuration=250ms in Phase 5 (matches smoke-nethermind-spamoor pacing)
func TestE2ESuite(t *testing.T) {
	runE2ESuite(t, "osaka")
}

// TestE2ESuiteAmsterdam runs the same suite on an Amsterdam genesis
// (Sepolia spec) against pinnedNethAmsterdamImage. On top of the Osaka
// suite it proves that Nethermind accepts the Amsterdam chainspec keys,
// the Amsterdam header fields (blockAccessListHash, slotNumber) and the
// Amsterdam system contracts. The mock CL then builds Amsterdam blocks
// with engine_forkchoiceUpdatedV4, getPayloadV6 and newPayloadV5.
func TestE2ESuiteAmsterdam(t *testing.T) {
	// The cross-client aggregator compares nethermind-result.json with the
	// Osaka roots of the other clients. The Amsterdam system contracts
	// change the root, so this run must not write that file (CI's
	// `-run TestE2ESuite` filter matches both tests).
	t.Setenv("RESULT_PATH", "")
	runE2ESuite(t, "amsterdam")
}

func runE2ESuite(t *testing.T, fork string) {
	if testing.Short() {
		t.Skip("e2e suite skipped in short mode")
	}

	const (
		seed = int64(42)
		// e2eBudget targets ~100 MB of synthetic trie state (matching the
		// pre-rewrite NumContracts=15_000 warmup).
		e2eBudget uint64 = 100 << 20
	)

	// 60M gas tracks current mainnet.
	g, err := stategenesis.BuildSynthetic(fork, big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "NETH")
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
	// every schema variant (including the spamoor sender). The Spec is
	// also passed to RunSuitePhases for the Phase 4 erc20 oracle.
	specDoc, preAlloc := e2e.LoadCISpec(t, "../../examples/full-matrix-spec-feature.yaml", "nethermind")
	cfg.PreAlloc = preAlloc
	// Deploy EIP-4788/2935/7002/7251 system contracts at their canonical
	// addresses — required for the cross-client genesis-root invariant.
	syscontracts.AddCanonicalSystemContracts(&cfg)
	if stategenesis.ForkAtLeast(fork, "amsterdam") {
		syscontracts.AddAmsterdamSystemContracts(&cfg)
	}

	if _, err := Run(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Write the e2e nethermind config into the datadir. Resolve chainspec
	// + datadir paths against the *container-side* layout (DinD: container
	// sees /data/<testName>; direct: /data).
	cfgPath := filepath.Join(dd.HostPath, "boot.cfg")
	chainSpecPath := dd.ContainerDatadir + "/" + ChainSpecFileName
	baseDbPath := dd.ContainerDatadir
	cfgContent := fmt.Sprintf(nethermindE2EConfigTemplate, chainSpecPath, baseDbPath)
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("write boot.cfg: %v", err)
	}

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:     seed,
		AutoFill: plan,
	})

	imageRef := nethImageRef(fork)
	containerPrefix := "state-actor-neth-boot-"
	engineFork := engineapi.ForkOsaka
	if stategenesis.ForkAtLeast(fork, "amsterdam") {
		containerPrefix = "neth-am-boot-"
		engineFork = engineapi.ForkAmsterdam
	}
	containerName := containerPrefix + e2e.RandSuffix(8)
	containerCfgPath := dd.ContainerDatadir + "/boot.cfg"
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("NETH_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"--config", containerCfgPath,
		// Nethermind requires flat to be opted into at boot even for a flat DB.
		"--FlatDb.Enabled=true",
		"--log", "Info",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("nethermind container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("nethermind container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()     //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nnethermind logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("nethermind JSON-RPC: %s", rpcURL)

	// Nethermind's .NET startup is comparable to Besu; allow 180s.
	if err := rpcprobe.WaitForRPC(rpcURL, 180*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup): %v", err)
	}

	// Flat-DB detection guard: Nethermind must select the flat backend for the
	// DB state-actor wrote; a silent patricia fallback would serve empty state.
	if logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput(); !strings.Contains(string(logs), "State backend: flat") {
		t.Fatalf("nethermind did not log 'State backend: flat' (flat DB not detected); logs:\n%s", logs)
	}

	// Mock CL: drive block production via engine API. MergePlugin's
	// seal-engine allowlist is {BeaconChain, Clique, Ethash}, so on an
	// Ethash chainspec with Merge.Enabled + TTD=0 the engine API is the
	// only path that produces blocks. Goroutine runs for the rest of
	// the test so spamoor's txs (Phase 5) get included in blocks.
	e2e.StartEngineDriver(t, containerIP, rpcURL, engineFork)

	// Phases 3-7: shared via internal/e2e.RunSuitePhases.
	e2e.RunSuitePhases(t, e2e.SuitePhasesCfg{
		ClientName:          "nethermind",
		RPCURL:              rpcURL,
		EOAs:                eoas,
		Contracts:           contracts,
		GeneratorConfig:     &cfg,
		Spec:                specDoc,
		SpecSeed:            e2e.CISpecSeed,
		SpamoorSlotDuration: 250 * time.Millisecond,
	})
}
