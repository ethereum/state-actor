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

	"github.com/ethereum/go-ethereum/common"

	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/oracle"
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

// spamoorSenderAddr / spamoorSenderPrivKey are the conventional dev key 1
// (privkey = 0x000…001 → addr = 0x7e5f4552…), matching SMOKE_INJECT_ADDRS
// in the Makefile. state-actor pre-funds it via cfg.InjectAddresses; the
// e2e suite hands the privkey to spamoor as the deployer wallet.
var (
	spamoorSenderAddr    = common.HexToAddress("0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")
	spamoorSenderPrivKey = "0x0000000000000000000000000000000000000000000000000000000000000001"
)

// TestE2ESuite is the per-PR end-to-end gate for the besu writer + boot
// path. Phases run sequentially with GHA-natural fail-fast (each step is
// a t.Fatalf on error, so phase N+1 doesn't run if N failed):
//
//  1. Generate a small state-actor datadir (10 EOAs + 3 contracts +
//     1 inject account) via besu.Run.
//  2. Boot hyperledger/besu against the datadir; wait for RPC.
//  3. Capture genesis state-root via eth_getBlockByNumber("0x0") and
//     emit it to $RESULT_PATH (the cross-client aggregator job
//     downloads + compares this across all 4 clients).
//  4. Re-query at "0x0": every entitygen-injected balance / code /
//     storage slot matches the writer-side reproduction.
//  5. Run spamoor (erc20_bloater) until tip advances by 100 blocks —
//     forces the node to mine + execute real txs, proving the DB is
//     writable, not just bootable.
//  6. Re-query at "latest": entitygen entities are unchanged (spamoor
//     touches a separate address space), and the spamoor sender's
//     nonce > 0 (proves it actually sent txs).
//  7. Update $RESULT_PATH with post-spamoor fields.
//
// Build-tagged `cgo_besu && oracle`. Run via `make test-besu-suite`;
// not included in plain `go test ./...`. Spamoor binary must be
// resolvable via $SPAMOOR or `command -v spamoor`; absent → t.Skip.
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

	// All 4 clients pin --fork=osaka post-Pre-C-v2. state-actor's
	// MaxForkForClient(c)=="osaka" for every client; pre-Prague forks
	// are EOL and rejected at parse. 60M gas tracks current mainnet.
	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := acquireOracleDatadir(t)
	defer cleanup()

	cfg := generator.Config{
		DBPath:          dd.hostPath,
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
		InjectAddresses: []common.Address{spamoorSenderAddr},
	}

	if _, err := Run(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Reproduce the RNG sequence state-actor's besu Phase 1 used so we
	// know the expected balances/code/storage. Single source of truth
	// in internal/oracle.Reproduce — same draw order across all 4
	// per-client boot tests + the e2e suites.
	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:         seed,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Distribution: cfg.Distribution,
	})

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
		"--min-gas-price", "0",
		// Post-Merge dev mode: besu has no native dev-mode block
		// production (clique is broken post-Shanghai per
		// hyperledger/besu#8532, removed in 26.4.0). Phase 3-4
		// (genesis-root + entity check at "0x0") works without
		// block production. Phase 5-7 (spamoor) requires an engine-
		// API mock CL — tracked as a follow-up; gated below with
		// t.Skip.
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

	// ---- Phase 3: capture genesis state-root → $RESULT_PATH ----
	genesisRoot, err := rpcprobe.GenesisStateRoot(rpcURL)
	if err != nil {
		t.Fatalf("GenesisStateRoot: %v", err)
	}
	t.Logf("genesis stateRoot: %s", genesisRoot.Hex())

	result := oracle.SuiteResult{
		ClientName:       "besu",
		GenesisStateRoot: genesisRoot,
	}
	if err := oracle.WriteResult(result); err != nil {
		t.Fatalf("WriteResult (pre-spamoor): %v", err)
	}

	// ---- Phase 4: oracle re-query at "0x0" ----
	checkEntities := func(blockTag string) (passed bool) {
		passed = true
		for _, eoa := range eoas {
			got, err := rpcprobe.EthGetBalance(rpcURL, eoa.Address, blockTag)
			if err != nil {
				t.Errorf("[%s] eth_getBalance %s: %v", blockTag, eoa.Address.Hex(), err)
				passed = false
				continue
			}
			want := eoa.StateAccount.Balance.ToBig()
			if got.Cmp(want) != 0 {
				t.Errorf("[%s] eth_getBalance %s: got %s want %s",
					blockTag, eoa.Address.Hex(), got.String(), want.String())
				passed = false
			}
		}
		for _, c := range contracts {
			gotCode, err := rpcprobe.EthGetCode(rpcURL, c.Address, blockTag)
			if err != nil {
				t.Errorf("[%s] eth_getCode %s: %v", blockTag, c.Address.Hex(), err)
				passed = false
			} else if !bytes.Equal(gotCode, c.Code) {
				t.Errorf("[%s] eth_getCode %s: len got=%d want=%d (first 32 bytes: got=%x want=%x)",
					blockTag, c.Address.Hex(), len(gotCode), len(c.Code),
					safePrefix(gotCode, 32), safePrefix(c.Code, 32))
				passed = false
			}
			for _, slot := range c.Storage {
				got, err := rpcprobe.EthGetStorageAt(rpcURL, c.Address, slot.Key, blockTag)
				if err != nil {
					t.Errorf("[%s] eth_getStorageAt %s slot %s: %v",
						blockTag, c.Address.Hex(), slot.Key.Hex(), err)
					passed = false
					continue
				}
				if got != slot.Value {
					t.Errorf("[%s] eth_getStorageAt %s slot %s: got %s want %s",
						blockTag, c.Address.Hex(), slot.Key.Hex(), got.Hex(), slot.Value.Hex())
					passed = false
				}
			}
		}
		return passed
	}
	if !checkEntities("0x0") {
		t.Fatalf("genesis-state oracle re-query failed; aborting before spamoor phase")
	}

	// ---- Phase 5: spamoor for ~100 blocks ----
	// Besu post-Merge dev-mode block production requires an engine-API
	// mock CL (clique is broken post-Shanghai per hyperledger/besu#8532).
	// The mock CL is a focused follow-up; for now Phase 5-7 is skipped
	// for besu while Phase 3-4 (the cross-client state-root invariant)
	// continues to gate every PR.
	t.Skip("besu Phase 5-7 (spamoor) skipped: engine-API mock CL is a follow-up")
	spamoorBin := os.Getenv("SPAMOOR")
	if spamoorBin == "" {
		spamoorBin = "spamoor"
	}
	if _, err := exec.LookPath(spamoorBin); err != nil {
		if os.Getenv("REQUIRE_SPAMOOR") == "1" {
			t.Fatalf("REQUIRE_SPAMOOR=1 but spamoor binary not found: %v", err)
		}
		t.Skipf("spamoor binary not found (set $SPAMOOR or `make spamoor-install`): %v", err)
	}
	postBlock, err := oracle.SpamoorRun(oracle.SpamoorRunCfg{
		Binary:           spamoorBin,
		RPCURL:           rpcURL,
		PrivKey:          spamoorSenderPrivKey,
		Seed:             12345,
		TargetBlockDelta: 100,
		SlotDuration:     time.Second,
		WalletCount:      5,
		TargetGasRatio:   0.1,
		Timeout:          5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SpamoorRun: %v", err)
	}
	t.Logf("post-spamoor tip: block %d", postBlock)
	result.PostSpamoorBlock = postBlock

	// ---- Phase 6: post-spamoor RPC re-query at "latest" ----
	if checkEntities("latest") {
		result.PostSpamoorEntityCheck = "ok"
	} else {
		result.PostSpamoorEntityCheck = "entitygen entities drifted post-spamoor"
	}

	deployerNonce, err := rpcprobe.EthGetTransactionCount(rpcURL, spamoorSenderAddr, "latest")
	if err != nil {
		t.Errorf("eth_getTransactionCount %s: %v", spamoorSenderAddr.Hex(), err)
	} else {
		result.PostSpamoorDeployerNonce = deployerNonce
		if deployerNonce == 0 {
			t.Errorf("post-spamoor deployer nonce is 0 — spamoor didn't send any txs?")
		}
	}

	// ---- Phase 7: write final result JSON ----
	if err := oracle.WriteResult(result); err != nil {
		t.Fatalf("WriteResult (post-spamoor): %v", err)
	}
}
