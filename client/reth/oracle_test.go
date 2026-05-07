//go:build cgo_reth && oracle

package reth

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	mrand "math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/oracle"
	iReth "github.com/nerolation/state-actor/internal/reth"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// oracleDatadir holds paths for an oracle test's datadir.
type oracleDatadir struct {
	// hostPath is the path on the test-container (or host) filesystem where
	// RunCgo writes the datadir.
	hostPath string
	// volMount is the -v argument for `docker run`: either "volname:/data"
	// (named-volume DinD mode) or "hostpath:/data" (direct mode).
	volMount string
	// containerDatadir is the path to pass as --datadir to the reth container.
	// In named-volume DinD mode this is "/data/<subdir>"; in direct mode it
	// is "/data".
	containerDatadir string
}

// acquireOracleDatadir returns an oracleDatadir for the calling test and a
// cleanup function. It honours the RETH_ORACLE_DATADIR / RETH_ORACLE_VOL
// env vars that make test-reth-oracle injects when running inside Docker
// (docker-in-docker via socket mount).
//
// When RETH_ORACLE_DATADIR is set, a unique sub-directory named after the
// test is created inside it so that multiple tests sharing the same named
// volume do not collide. The reth container is pointed at the sub-path inside
// the mounted volume.
//
// When neither env var is set the function falls back to t.TempDir() —
// suitable for direct host runs if libmdbx is available.
func acquireOracleDatadir(t *testing.T) (oracleDatadir, func()) {
	t.Helper()
	baseDir := os.Getenv("RETH_ORACLE_DATADIR")
	if baseDir == "" {
		datadir := t.TempDir()
		return oracleDatadir{
			hostPath:         datadir,
			volMount:         datadir + ":/data",
			containerDatadir: "/data",
		}, func() {}
	}

	// Derive a unique sub-directory from the test name so each test gets its
	// own fresh datadir even though they share the same named volume.
	subName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	hostPath := baseDir + "/" + subName
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		t.Fatalf("acquireOracleDatadir: mkdir %s: %v", hostPath, err)
	}
	t.Logf("using datadir=%s", hostPath)

	vol := os.Getenv("RETH_ORACLE_VOL")
	volMount := hostPath + ":/data"
	containerDatadir := "/data"
	if vol != "" {
		// Named-volume DinD mode: mount the full named volume at /data and
		// point reth at the per-test sub-path within it.
		volMount = vol + ":/data"
		containerDatadir = "/data/" + subName
	}

	return oracleDatadir{
		hostPath:         hostPath,
		volMount:         volMount,
		containerDatadir: containerDatadir,
	}, func() {}
}

// rethImageRef returns the fully-qualified reth image reference from the
// pinned constants, falling back to known-good defaults.
func rethImageRef() string {
	image := iReth.PinnedRethImage
	if image == "" {
		image = "ghcr.io/paradigmxyz/reth"
	}
	tag := iReth.PinnedRethRelease
	if tag == "" {
		tag = "v2.1.0"
	}
	return image + ":" + tag
}

// dockerPlatformArgs returns ["--platform", $RETH_DOCKER_PLATFORM] when the
// env var is set, otherwise an empty slice. Use to inject `--platform
// linux/amd64` (qemu emulation) on arm64 hosts when the pinned image lacks
// an arm64 manifest. See nerolation/state-actor#43.
func dockerPlatformArgs() []string {
	if v := os.Getenv("RETH_DOCKER_PLATFORM"); v != "" {
		return []string{"--platform", v}
	}
	return nil
}

// parseDbStatsEntries extracts the numeric entry count for table from the
// output of `reth db stats`. Returns (count, ok). The output format uses pipe
// separators; the table name appears in column 1 and the entry count in
// column 2.
//
// Conservative: returns false if the format doesn't match expectations.
func parseDbStatsEntries(output, table string) (int, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, table) {
			continue
		}
		fields := strings.Fields(strings.ReplaceAll(line, "|", " "))
		// Find the table name field, then take the next numeric field as count.
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

// TestRethDbStats generates an empty-alloc datadir via RunCgo, then invokes
// the stock paradigmxyz/reth Docker image's `db stats` subcommand against
// it. If our datadir layout is structurally invalid (wrong page size,
// missing tables, wrong schema version), `db stats` exits non-zero.
//
// Gated by both `cgo_reth` AND `oracle` build tags. Run via
// `make test-reth-oracle` — the plain `go test` does not include either tag.
//
// When running inside Docker (docker-in-docker via socket mount), the test
// process's tmp path is not directly accessible to the host Docker daemon.
// Set RETH_ORACLE_DATADIR to a directory that is visible to both the test
// container and the Docker daemon (e.g. a host path bind-mounted into both
// containers, or a Docker named-volume mount point). make test-reth-oracle
// sets this automatically.
func TestRethDbStats(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle test in short mode")
	}

	dd, cleanup := acquireOracleDatadir(t)
	defer cleanup()

	cfg := generator.Config{DBPath: dd.hostPath}
	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	args := append([]string{"run", "--rm"}, dockerPlatformArgs()...)
	args = append(args,
		"-v", dd.volMount,
		rethImageRef(),
		"db", "--datadir", dd.containerDatadir, "stats",
	)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reth db stats failed:\noutput:\n%s\nerr: %v", out, err)
	}

	// Sanity: check the output mentions some expected tables.
	output := string(out)
	for _, table := range []string{"PlainAccountState", "HashedAccounts", "Bytecodes"} {
		if !strings.Contains(output, table) {
			t.Errorf("expected table %q in db stats output, got:\n%s", table, output)
		}
	}
}

// TestRethDbStatsSyntheticEOAs generates a 100-EOA datadir and verifies
// reth's `db stats` shows the expected row counts in the EOA-touched tables.
func TestRethDbStatsSyntheticEOAs(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle test in short mode")
	}

	const numAccounts = 100

	dd, cleanup := acquireOracleDatadir(t)
	defer cleanup()

	cfg := generator.Config{
		DBPath:      dd.hostPath,
		NumAccounts: numAccounts,
		Seed:        12345,
	}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats.AccountsCreated != numAccounts {
		t.Fatalf("AccountsCreated = %d, want %d", stats.AccountsCreated, numAccounts)
	}

	args := append([]string{"run", "--rm"}, dockerPlatformArgs()...)
	args = append(args,
		"-v", dd.volMount,
		rethImageRef(),
		"db", "--datadir", dd.containerDatadir, "stats",
	)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reth db stats failed:\noutput:\n%s\nerr: %v", out, err)
	}

	output := string(out)

	// Verify the four EOA-touched tables show >= numAccounts entries.
	checks := map[string]int{
		"PlainAccountState": numAccounts,
		"HashedAccounts":    numAccounts,
		"AccountChangeSets": numAccounts,
		"AccountsHistory":   numAccounts,
	}
	for table, minEntries := range checks {
		count, ok := parseDbStatsEntries(output, table)
		if !ok {
			t.Errorf("could not parse entry count for %q from db stats output:\n%s", table, output)
			continue
		}
		if count < minEntries {
			t.Errorf("table %q: %d entries, want >= %d", table, count, minEntries)
		}
	}
}

// ---------------------------------------------------------------------------
// TestRethNodeBootEmptyAlloc — diagnostic: verify genesis hash for empty state
// ---------------------------------------------------------------------------

// TestRethNodeBootEmptyAlloc generates a datadir with no accounts and boots
// reth node --dev. This tests whether our genesis header (with empty state root)
// produces a genesis hash that reth accepts. If this fails, the issue is in
// header field encoding (not account/storage state). If this passes, the issue
// is specific to non-empty alloc.
func TestRethNodeBootEmptyAlloc(t *testing.T) {
	if testing.Short() {
		t.Skip("oracle boot test skipped in short mode")
	}

	dd, cleanup := acquireOracleDatadir(t)
	defer cleanup()

	cfg := generator.Config{
		DBPath: dd.hostPath,
		// No accounts, no contracts — uses empty MPT state root.
	}

	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	imageRef := rethImageRef()
	containerName := "state-actor-reth-boot-empty-" + randSuffix(8)
	chainspecPath := dd.containerDatadir + "/chainspec.json"
	runArgs := append([]string{"run", "-d"}, dockerPlatformArgs()...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.volMount,
		imageRef,
		"node", "--dev",
		"--chain", chainspecPath,
		"--datadir", dd.containerDatadir,
		// state-actor's chainspec.json carries an empty alloc; the genesis
		// state was direct-written into MDBX. Tell reth to trust the DB.
		"--debug.skip-genesis-validation",
		"--http",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth",
	)
	runCmd := exec.Command("docker", runArgs...)
	runOut, err := runCmd.CombinedOutput()
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

	containerIP, err := inspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("inspectContainerIP: %v\nreth logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("reth JSON-RPC: %s", rpcURL)

	if err := rpcprobe.WaitForRPC(rpcURL, 120*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs in t.Cleanup):\n%v", err)
	}
	t.Log("empty-alloc reth node booted successfully")
}

// ---------------------------------------------------------------------------
// TestE2ESuite — per-PR end-to-end gate
// ---------------------------------------------------------------------------

// spamoorSenderAddr / spamoorSenderPrivKey — conventional dev key 1
// (privkey = 0x000…001 → addr = 0x7e5f4552…). state-actor pre-funds via
// cfg.InjectAddresses; spamoor uses the privkey as deployer.
var (
	spamoorSenderAddr    = common.HexToAddress("0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")
	spamoorSenderPrivKey = "0x0000000000000000000000000000000000000000000000000000000000000001"
)

// TestE2ESuite — see client/besu/e2e_test.go for the full phase
// description. reth-specific bits:
//   - DinD via the `oracle` build tag.
//   - Pinned image: paradigmxyz/reth:nightly (post-#23919, supports
//     --debug.skip-genesis-validation; was the CPerezz/reth fork before
//     that PR landed upstream).
//   - --dev mode auto-mines on tx, so spamoor can advance the chain
//     without an external CL.
//
// Wall-time budget: up to 120s for reth to start; spamoor adds ~1-2 min.
// Build-tagged `cgo_reth && oracle`. Run via `make test-reth-suite`.
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

	// Pin --fork=osaka (reth's MaxForkForClient ceiling, post-PR-Pre-C).
	// 60M gas matches mainnet-current. internal/genesisheader.Build's
	// existing Prague ladder is structurally complete for Osaka.
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
		Genesis:         g,
		InjectAddresses: []common.Address{spamoorSenderAddr},
	}

	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	// Reproduce the RNG sequence to capture expected values. Goes through
	// entitygen.GenerateContractRoll (same canonical draw order RunCgo
	// uses): for each contract, GenerateSlotCount(rng, ...) THEN
	// GenerateContract(rng, ...). The previous reproduction here computed
	// a static slotCount mid-point and called GenerateContract directly,
	// leaving the RNG one Float64 ahead of RunCgo per contract — every
	// contract address it derived was wrong, and every contract probe
	// would silently return zero. The geth boot test surfaced this by
	// asserting eth_getCode and eth_getStorageAt; nerolation/state-actor#42.
	// Single source of truth in internal/oracle.Reproduce — same draw
	// order across all 4 per-client boot tests + e2e suites.
	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:         seed,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Distribution: cfg.Distribution,
	})

	// Boot reth node --dev.
	imageRef := rethImageRef()
	containerName := "state-actor-reth-boot-" + randSuffix(8)

	// Do NOT use --rm: we need to capture logs even if reth exits immediately.
	// Cleanup removes the container explicitly after capturing logs.
	// Do NOT publish the port to the host (-p). When this test runs inside a
	// Docker container (DinD via socket mount), host-published ports are bound
	// on the Docker host VM — not reachable from inside our test container.
	// Instead we obtain the reth container's bridge IP via `docker inspect`
	// and connect directly on port 8545. This works because all containers
	// sharing the Docker daemon's default bridge network can reach each other
	// by IP.
	//
	// --chain points at the chainspec.json that RunCgo persisted in the datadir.
	// Without this, reth defaults to its built-in --dev chainspec whose genesis
	// hash won't match our custom-written datadir.
	chainspecPath := dd.containerDatadir + "/chainspec.json"
	runArgs := append([]string{"run", "-d"}, dockerPlatformArgs()...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.volMount,
		imageRef,
		"node", "--dev",
		"--chain", chainspecPath,
		"--datadir", dd.containerDatadir,
		// state-actor's chainspec.json carries an empty alloc; the genesis
		// state was direct-written into MDBX. Tell reth to trust the DB.
		"--debug.skip-genesis-validation",
		"--http",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth",
	)
	runCmd := exec.Command("docker", runArgs...)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("reth container started: %s", strings.TrimSpace(string(runOut)))

	// Ensure the container is stopped and removed when the test finishes.
	// Capture logs first for diagnosis.
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("reth container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()  //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	// Resolve the reth container's bridge IP so we can reach it from inside
	// our own container (or from the host when running locally).
	containerIP, err := inspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("inspectContainerIP: %v\nreth logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("reth JSON-RPC: %s", rpcURL)

	// Poll until the RPC endpoint is accepting connections (max 120 s).
	if err := rpcprobe.WaitForRPC(rpcURL, 120*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup):\nerr: %v", err)
	}

	// ---- Phase 3: capture genesis state-root → $RESULT_PATH ----
	genesisRoot, err := rpcprobe.GenesisStateRoot(rpcURL)
	if err != nil {
		t.Fatalf("GenesisStateRoot: %v", err)
	}
	t.Logf("genesis stateRoot: %s", genesisRoot.Hex())
	result := oracle.SuiteResult{
		ClientName:       "reth",
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

// ---------------------------------------------------------------------------
// Container helpers
// ---------------------------------------------------------------------------

// inspectContainerIP returns the bridge-network IP of a running container.
// This is the IP that other containers (and the host when running natively)
// can use to reach the container's exposed ports without a host port mapping.
// In Docker-in-Docker (socket-mount) mode this is the only reliable way to
// reach the spawned reth container from inside the test container — host port
// mappings are bound on the Docker VM, not the test container's network.
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

// randSuffix returns a random lower-hex suffix of length n for container names.
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
