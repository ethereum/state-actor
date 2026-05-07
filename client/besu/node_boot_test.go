//go:build cgo_besu && oracle

package besu

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	mrand "math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// pinnedBesuImage is the upstream Besu Docker tag the boot test pins
// against. Override with BESU_IMAGE=hyperledger/besu:vX.Y.Z to test a
// pre-release version. Matches the default in
// client/besu/testdata/validate-big-db-besu.sh so smoke + oracle tests
// exercise the same release.
const pinnedBesuImage = "hyperledger/besu:25.11.0"

func besuImageRef() string {
	if v := os.Getenv("BESU_IMAGE"); v != "" {
		return v
	}
	return pinnedBesuImage
}

// dockerPlatformArgs returns ["--platform", $BESU_DOCKER_PLATFORM] when the
// env var is set, otherwise nil. Mirrors the reth + geth platform-override
// pattern (see nerolation/state-actor#43).
func dockerPlatformArgs() []string {
	if v := os.Getenv("BESU_DOCKER_PLATFORM"); v != "" {
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
// In DinD mode (test runs inside a container with /var/run/docker.sock
// mounted) the spawned besu container's host-port mapping is on the Docker
// host VM, not on the test container's network — but both containers share
// the Docker daemon's default bridge network, so they can reach each other
// by bridge IP.
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

// oracleDatadir holds paths for an oracle test's datadir. Mirrors the
// reth-side struct in client/reth/oracle_test.go.
type oracleDatadir struct {
	hostPath         string
	volMount         string
	containerDatadir string
}

// acquireOracleDatadir returns an oracleDatadir for the calling test and a
// cleanup function. Honours BESU_ORACLE_DATADIR / BESU_ORACLE_VOL env vars
// that `make test-besu-boot` injects when running inside Docker (DinD via
// socket mount). Falls back to t.TempDir() for direct host runs.
func acquireOracleDatadir(t *testing.T) (oracleDatadir, func()) {
	t.Helper()
	baseDir := os.Getenv("BESU_ORACLE_DATADIR")
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

	vol := os.Getenv("BESU_ORACLE_VOL")
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

// TestBesuNodeBoot generates a small state-actor datadir (10 EOAs +
// 3 contracts) via Run, boots upstream hyperledger/besu against it, and
// probes via JSON-RPC to confirm:
//
//   - eth_getBalance matches every EOA's generated balance
//   - eth_getCode matches every contract's bytecode
//   - eth_getStorageAt matches every contract's storage slots
//
// Uses the same canonical RNG draw order (entitygen.GenerateContractRoll)
// the writer uses, so the reproduced contracts match the persisted ones.
//
// Build-tagged `cgo_besu && oracle`. Run via `make test-besu-boot`; not
// included in plain `go test`.
func TestBesuNodeBoot(t *testing.T) {
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

	// Pin --fork=shanghai. state-actor's BuildSynthetic does not emit a
	// BlobSchedule, which Besu requires once Cancun or Prague are active
	// at the genesis chain config. Shanghai is besu's writer ceiling
	// today (genesis.MaxForkForClient("besu") == "shanghai").
	g, err := stategenesis.BuildSynthetic("shanghai", big.NewInt(1337), 30_000_000, 0, nil)
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

	// Reproduce the RNG sequence state-actor's besu Phase 1 used so we
	// know the expected balances/code/storage. Goes through
	// entitygen.GenerateContractRoll — same canonical draw order
	// (slot-count then contract) the writer uses.
	rng := mrand.New(mrand.NewSource(seed))
	eoas := make([]*entitygen.Account, numAccounts)
	for i := 0; i < numAccounts; i++ {
		eoas[i] = entitygen.GenerateEOA(rng)
	}
	contracts := make([]*entitygen.Account, numContracts)
	for i := 0; i < numContracts; i++ {
		contracts[i] = entitygen.GenerateContractRoll(rng, cfg.Distribution, codeSize, minSlots, maxSlots)
	}

	// Boot upstream Besu. --genesis-state-hash-cache-enabled tells Besu to
	// trust the stored stateRoot rather than recompute it from the small
	// genesis JSON's alloc — necessary because state-actor writes
	// synthetic state separately from what the chainspec's alloc would
	// produce. Same pattern validate-big-db-besu.sh uses.
	imageRef := besuImageRef()
	containerName := "state-actor-besu-boot-" + randSuffix(8)
	chainspecPath := dd.containerDatadir + "/" + ChainSpecFileName
	runArgs := append([]string{"run", "-d"}, dockerPlatformArgs()...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.volMount,
		imageRef,
		"--data-path", dd.containerDatadir,
		"--genesis-file", chainspecPath,
		"--network-id", "1337",
		"--data-storage-format", "BONSAI",
		"--genesis-state-hash-cache-enabled",
		"--rpc-http-enabled",
		"--rpc-http-host", "0.0.0.0",
		"--rpc-http-port", "8545",
		"--rpc-http-api", "ETH,NET,WEB3",
		// `--host-allowlist=all` literally accepts any Host: header. We
		// don't pass `*` because the besu Docker image's entrypoint script
		// expands unquoted `$@`, which globs `*` against /opt/besu's
		// directory contents (README.md, bin, lib, …) — first run failed
		// with "Unmatched arguments from index 18: 'README.md', …". `all`
		// has no glob characters so it survives intact. Per besu docs
		// (`--host-allowlist=...`), "all" and "*" are equivalent literals.
		"--host-allowlist", "all",
		// Miner / dev-mode flags. Required for besu 25.11.0 to accept a
		// chainspec with ethash.fixeddifficulty (state-actor's
		// chainspec.go writes that stanza to enable
		// post-london-no-CL block production). Without --miner-enabled +
		// --miner-coinbase, besu rejects the chainspec at the
		// "Supplied genesis block does not match chain data stored"
		// check even with --genesis-state-hash-cache-enabled. Same flag
		// set client/besu/testdata/validate-big-db-besu.sh uses.
		"--min-gas-price", "0",
		"--miner-enabled",
		"--miner-coinbase", "0x0000000000000000000000000000000000000000",
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

	containerIP, err := inspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("inspectContainerIP: %v\nbesu logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("besu JSON-RPC: %s", rpcURL)

	// Besu's JVM startup is slower than geth's Go startup — give it 180s.
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
