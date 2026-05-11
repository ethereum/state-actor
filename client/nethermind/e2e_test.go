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

	"github.com/ethereum/go-ethereum/common"

	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/oracle"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// pinnedNethImage is the upstream Nethermind Docker tag the e2e suite
// pins against. Override with NETH_IMAGE=nethermind/nethermind:vX.Y.Z to
// test a pre-release version.
const pinnedNethImage = "nethermind/nethermind:1.37.0"

func nethImageRef() string {
	if v := os.Getenv("NETH_IMAGE"); v != "" {
		return v
	}
	return pinnedNethImage
}

// nethermindE2EConfigTemplate is the Nethermind config for the e2e suite
// (boot + spamoor mining). Mirrors client/nethermind/testdata/configs/
// sa-dev-v2.json but inlined here so the test is self-contained — written
// into the datadir at runtime with the chainspec / DB paths plugged in,
// so DinD mode (where the test container places state-actor data at
// /data/<testName>/) and direct mode (/data) both resolve correctly.
//
// Mining.Enabled = true and EnableUnsecuredDevWallet = true so spamoor's
// --privkey deployer can submit txs and advance the chain.
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
    "NetworkingEnabled": false,
    "SynchronizationEnabled": false
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
    "EnabledModules": ["Eth", "Net", "Web3"]
  },
  "Metrics": {"Enabled": false},
  "Merge": {"Enabled": true, "TerminalTotalDifficulty": "0"},
  "Mining": {"Enabled": true}
}`

// TestE2ESuite — see client/besu/e2e_test.go for the full phase
// description. Phase 1-2 (datadir + Run + boot nethermind) is per-client;
// Phase 3-7 is internal/oracle.RunSuitePhases.
//
// nethermind-specific bits:
//   - --fork=osaka (unified across all 4 clients after the writer migration to internal/genesisheader.Build)
//   - inline boot.cfg with Mining.Enabled=true (NethDev produces blocks)
//   - SlotDuration=250ms in Phase 5 (matches smoke-nethermind-spamoor pacing)
func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e suite skipped in short mode")
	}

	const (
		seed         = int64(42)
		numAccounts  = 10
		numContracts = 3
		codeSize     = 256
		minSlots     = 2
		maxSlots     = 2
	)

	// All 4 clients pin --fork=osaka after the writer migration to internal/genesisheader.Build. state-actor's
	// MaxForkForClient(c)=="osaka" for every client; pre-Prague forks
	// are EOL and rejected at parse. 60M gas tracks current mainnet.
	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := oracle.AcquireDatadir(t, "NETH")
	defer cleanup()

	cfg := generator.Config{
		DBPath:          dd.HostPath,
		NumAccounts:     numAccounts,
		NumContracts:    numContracts,
		CodeSize:        codeSize,
		MinSlots:        minSlots,
		MaxSlots:        maxSlots,
		Seed:            seed,
		BatchSize:       1000,
		Workers:         1,
		TrieMode:        generator.TrieModeMPT,
		Genesis:         g,
		InjectAddresses: []common.Address{oracle.SpamoorSenderAddr},
	}
	// Deploy EIP-4788/2935/7002/7251 system contracts at their canonical
	// addresses — required for the cross-client genesis-root invariant.
	oracle.AddPragueSystemContracts(&cfg)

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
		Seed:         seed,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Distribution: cfg.Distribution,
	})

	imageRef := nethImageRef()
	containerName := "state-actor-neth-boot-" + oracle.RandSuffix(8)
	containerCfgPath := dd.ContainerDatadir + "/boot.cfg"
	runArgs := append([]string{"run", "-d"}, oracle.DockerPlatformArgs("NETH_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"--config", containerCfgPath,
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
		exec.Command("docker", "stop", containerName).Run()    //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := oracle.InspectContainerIP(containerName)
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

	// Phases 3-4 only — SkipBlockProduction because nethermind's NethDev
	// consensus engine doesn't produce blocks on a post-Prague chain
	// with the EIP-4788/2935/7002/7251 system contracts deployed in
	// genesis state (cf. ethereum/state-actor#56). Phase 3 still
	// captures the genesis stateRoot → result.json so the cross-client
	// genesis-root aggregator CI gate runs as expected. Proper fix:
	// switch nethermind e2e to the engine API + EngineDriver mock
	// (same as besu).
	oracle.RunSuitePhases(t, oracle.SuitePhasesCfg{
		ClientName:          "nethermind",
		RPCURL:              rpcURL,
		EOAs:                eoas,
		Contracts:           contracts,
		GeneratorConfig:     &cfg,
		SpamoorSlotDuration: 250 * time.Millisecond,
		SkipBlockProduction: true,
	})
}
