//go:build cgo_neth && oracle

package nethermind

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// pinnedNethImage is the upstream Nethermind Docker tag the boot test pins
// against. Override with NETH_IMAGE=nethermind/nethermind:vX.Y.Z to test a
// pre-release version. Matches the default in
// client/nethermind/testdata/validate-big-db.sh.
const pinnedNethImage = "nethermind/nethermind:1.37.0"

func nethImageRef() string {
	if v := os.Getenv("NETH_IMAGE"); v != "" {
		return v
	}
	return pinnedNethImage
}

// dockerPlatformArgs returns ["--platform", $NETH_DOCKER_PLATFORM] when the
// env var is set, otherwise nil. Mirrors reth + besu + geth platform-override
// pattern (see nerolation/state-actor#43).
func dockerPlatformArgs() []string {
	if v := os.Getenv("NETH_DOCKER_PLATFORM"); v != "" {
		return []string{"--platform", v}
	}
	return nil
}

// randSuffix returns a random lower-hex suffix of length n for unique
// container names.
func randSuffix(n int) string {
	const chars = "abcdef0123456789"
	r := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// safePrefix returns the first n bytes of b, or all of b if len(b) < n.
func safePrefix(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// inspectContainerIP returns the bridge-network IP of a running container.
// Used so the test can reach the spawned nethermind container by IP rather
// than by host port mapping (which doesn't work in DinD mode).
func inspectContainerIP(containerName string) (string, error) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		containerName,
	).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", containerName, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("container %s has no bridge IP (not yet started?)", containerName)
	}
	return ip, nil
}

// oracleDatadir holds paths for an oracle test's datadir.
type oracleDatadir struct {
	hostPath         string
	volMount         string
	containerDatadir string
}

// acquireOracleDatadir returns an oracleDatadir for the calling test and a
// cleanup function. Honours NETH_ORACLE_DATADIR / NETH_ORACLE_VOL env vars
// that `make test-nethermind-boot` injects when running inside Docker
// (DinD via socket mount). Falls back to t.TempDir() for direct host runs.
func acquireOracleDatadir(t *testing.T) (oracleDatadir, func()) {
	t.Helper()
	baseDir := os.Getenv("NETH_ORACLE_DATADIR")
	if baseDir == "" {
		datadir := t.TempDir()
		return oracleDatadir{
			hostPath:         datadir,
			volMount:         datadir + ":/data",
			containerDatadir: "/data",
		}, func() {}
	}
	subName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	hostPath := baseDir + "/" + subName
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		t.Fatalf("acquireOracleDatadir: mkdir %s: %v", hostPath, err)
	}
	t.Logf("using datadir=%s", hostPath)

	vol := os.Getenv("NETH_ORACLE_VOL")
	volMount := hostPath + ":/data"
	containerDatadir := "/data"
	if vol != "" {
		volMount = vol + ":/data"
		containerDatadir = "/data/" + subName
	}
	return oracleDatadir{
		hostPath:         hostPath,
		volMount:         volMount,
		containerDatadir: containerDatadir,
	}, func() {}
}

// nethermindBootConfigTemplate is the minimal Nethermind config for booting
// in passive read-only mode against a state-actor datadir. Mirrors
// client/nethermind/testdata/configs/sa-dev-v2.json but inlined here so the
// test is self-contained — written into the datadir at runtime with the
// chainspec / DB paths plugged in, so DinD mode (where the test container
// places state-actor data at /data/<testName>/) and direct mode (/data)
// both resolve correctly.
//
// Two %s placeholders, in order:
//   1. ChainSpecPath  — full path to state-actor's parity-chainspec.json
//   2. BaseDbPath     — full path to the per-test datadir
const nethermindBootConfigTemplate = `{
  "Init": {
    "EnableUnsecuredDevWallet": false,
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
  "Merge": {"Enabled": false},
  "Mining": {"Enabled": false}
}`

// TestNethermindNodeBoot generates a small state-actor datadir (10 EOAs +
// 3 contracts) via Run, boots upstream nethermind/nethermind against it,
// and probes via JSON-RPC to confirm balances / code / storage.
//
// Build-tagged `cgo_neth && oracle`. Run via `make test-nethermind-boot`;
// not included in plain `go test`.
func TestNethermindNodeBoot(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle boot test skipped in short mode")
	}

	const (
		seed         = int64(42)
		numAccounts  = 10
		numContracts = 3
		codeSize     = 256
		minSlots     = 2
		maxSlots     = 2
	)

	// Pin --fork=merge. Nethermind's writer ceiling today is "merge"
	// (genesis.MaxForkForClient("nethermind") == "merge"); going past
	// that is rejected at parse time in main.go.
	g, err := stategenesis.BuildSynthetic("merge", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := acquireOracleDatadir(t)
	defer cleanup()

	cfg := generator.Config{
		DBPath:       dd.hostPath,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Seed:         seed,
		BatchSize:    1000,
		Workers:      1,
		TrieMode:     generator.TrieModeMPT,
		Genesis:      g,
	}

	if _, err := Run(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Write the minimal nethermind config into the datadir, with paths
	// resolved against the *container-side* layout (dd.containerDatadir),
	// not the host-side path. In DinD mode they differ — the test
	// container's filesystem and the spawned nethermind container's
	// filesystem only share the named volume, mounted at /data on each;
	// the per-test sub-directory is /data/<testName>.
	cfgPath := filepath.Join(dd.hostPath, "boot.cfg")
	chainSpecPath := dd.containerDatadir + "/" + ChainSpecFileName
	baseDbPath := dd.containerDatadir
	cfgContent := fmt.Sprintf(nethermindBootConfigTemplate, chainSpecPath, baseDbPath)
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("write boot.cfg: %v", err)
	}

	// Reproduce the RNG sequence state-actor's neth Phase 1 used.
	rng := mrand.New(mrand.NewSource(seed))
	eoas := make([]*entitygen.Account, numAccounts)
	for i := 0; i < numAccounts; i++ {
		eoas[i] = entitygen.GenerateEOA(rng)
	}
	contracts := make([]*entitygen.Account, numContracts)
	for i := 0; i < numContracts; i++ {
		contracts[i] = entitygen.GenerateContractRoll(rng, cfg.Distribution, codeSize, minSlots, maxSlots)
	}

	imageRef := nethImageRef()
	containerName := "state-actor-neth-boot-" + randSuffix(8)
	containerCfgPath := dd.containerDatadir + "/boot.cfg"
	runArgs := append([]string{"run", "-d"}, dockerPlatformArgs()...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.volMount,
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

	containerIP, err := inspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("inspectContainerIP: %v\nnethermind logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("nethermind JSON-RPC: %s", rpcURL)

	// Nethermind's .NET startup is comparable to Besu; allow 180s.
	if err := rpcprobe.WaitForRPC(rpcURL, 180*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup):\nerr: %v", err)
	}

	// ---- EOA assertions ----
	for _, eoa := range eoas {
		got, err := rpcprobe.EthGetBalance(rpcURL, eoa.Address, "latest")
		if err != nil {
			t.Errorf("eth_getBalance %s: %v", eoa.Address.Hex(), err)
			continue
		}
		want := eoa.StateAccount.Balance.ToBig()
		if got.Cmp(want) != 0 {
			t.Errorf("eth_getBalance %s: got %s want %s",
				eoa.Address.Hex(), got.String(), want.String())
		}
	}

	// ---- contract assertions ----
	for _, c := range contracts {
		gotCode, err := rpcprobe.EthGetCode(rpcURL, c.Address, "latest")
		if err != nil {
			t.Errorf("eth_getCode %s: %v", c.Address.Hex(), err)
		} else if !bytes.Equal(gotCode, c.Code) {
			t.Errorf("eth_getCode %s: len got=%d want=%d (first 32 bytes: got=%x want=%x)",
				c.Address.Hex(), len(gotCode), len(c.Code),
				safePrefix(gotCode, 32), safePrefix(c.Code, 32))
		}

		for _, slot := range c.Storage {
			got, err := rpcprobe.EthGetStorageAt(rpcURL, c.Address, slot.Key, "latest")
			if err != nil {
				t.Errorf("eth_getStorageAt %s slot %s: %v",
					c.Address.Hex(), slot.Key.Hex(), err)
				continue
			}
			if got != slot.Value {
				t.Errorf("eth_getStorageAt %s slot %s: got %s want %s",
					c.Address.Hex(), slot.Key.Hex(), got.Hex(), slot.Value.Hex())
			}
		}
	}
}
