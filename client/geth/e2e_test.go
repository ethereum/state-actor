//go:build oracle

package geth

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	mrand "math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/oracle"
	"github.com/nerolation/state-actor/internal/rpcprobe"

	stategenesis "github.com/nerolation/state-actor/genesis"
)

// defaultGethImage is the upstream geth Docker tag the boot test pins
// against. Override with GETH_IMAGE=ethereum/client-go:vX.Y.Z to test a
// pre-release version or a fork. Matches the default in
// validate-big-db-geth.sh so smoke + oracle tests exercise the same
// release.
const defaultGethImage = "ethereum/client-go:v1.17.2"

func gethImageRef() string {
	if v := os.Getenv("GETH_IMAGE"); v != "" {
		return v
	}
	return defaultGethImage
}

// dockerPlatformArgs returns ["--platform", $GETH_DOCKER_PLATFORM] when
// the env var is set, otherwise an empty slice. Use to inject `--platform
// linux/amd64` (qemu emulation) on arm64 hosts when the geth image lacks
// an arm64 manifest, mirroring the reth side. Tracked in
// nerolation/state-actor#43.
func dockerPlatformArgs() []string {
	if v := os.Getenv("GETH_DOCKER_PLATFORM"); v != "" {
		return []string{"--platform", v}
	}
	return nil
}

// freeTCPPort asks the kernel for an available TCP port. The returned
// port is briefly bound and then released — there's a small race window
// before docker -p binds it back, but in practice this is the standard
// trick for picking parallel-test-safe ports.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// randSuffix returns a random lower-hex suffix of length n for container
// names. Mirrors the helper in client/reth/oracle_test.go.
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

// spamoorSenderAddr / spamoorSenderPrivKey are the conventional dev key 1
// (privkey = 0x000…001 → addr = 0x7e5f4552…). state-actor pre-funds it
// via cfg.InjectAddresses; spamoor uses the privkey as deployer.
var (
	spamoorSenderAddr    = common.HexToAddress("0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")
	spamoorSenderPrivKey = "0x0000000000000000000000000000000000000000000000000000000000000001"
)

// TestE2ESuite — see client/besu/e2e_test.go for the full phase
// description. geth-specific bits:
//   - --fork=osaka (matches all 4 clients post-Pre-C-v2). 60M gas
//     limit (mainnet-current).
//   - Boots geth in `--dev --dev.period=1` mode: post-Merge dev chain,
//     1s blocks, self-contained CL emulation. No engine-API mock needed
//     — geth's --dev wraps engine API internally and continues mining
//     on top of state-actor's pre-written DB (recipe lifted verbatim
//     from CPerezz/bintrie-benchmarks/.../generate_db.sh).
//   - No DinD; geth runs with -p host-port mapping.
//   - Phase 5-7 (spamoor + post-spamoor re-query) runs against the
//     same dev-mode chain, just like besu/neth/reth.
//
// Build-tagged `oracle`. Run via `make test-geth-suite`.
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

	// Datadir layout: state-actor writes to <datadir>/geth/chaindata; geth
	// itself takes --datadir=<datadir> and looks for geth/chaindata under
	// it. We mount the parent into the container at /data.
	datadir := t.TempDir()
	// InjectAddresses pre-funds the spamoor sender; same value all 4
	// e2e suites use so the cross-client genesis state-root invariant
	// holds (alloc shapes match across clients).
	cfg := generator.Config{
		DBPath:          filepath.Join(datadir, "geth", "chaindata"),
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
	if _, err := Populate(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Populate: %v", err)
	}

	// Reproduce the RNG sequence state-actor's geth Phase 1 used.
	// Single source of truth in internal/oracle.Reproduce — same draw
	// order across all 4 per-client e2e suites.
	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:         seed,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Distribution: generator.PowerLaw,
	})

	// Boot upstream geth in --dev mode (PoA, self-emulated CL).
	// --dev.period=1 mines blocks every 1s so spamoor advances the
	// chain quickly. --dev.gaslimit matches the genesis 60M ceiling.
	// --networkid matches the chainID embedded in state-actor's
	// genesis. --db.engine=pebble points geth at our Pebble datadir
	// (default is leveldb on geth ≤ v1.13). --http.api includes
	// txpool so spamoor can monitor pending txs.
	containerName := "state-actor-geth-boot-" + randSuffix(8)
	hostPort := freeTCPPort(t)

	// Run geth as the test process's UID/GID so any files it writes to
	// the bind-mounted /data stay owned by the test user. Without this,
	// on native Linux (e.g. GHA runners), geth's default-root container
	// would write root-owned files into datadir, and t.TempDir's cleanup
	// (running as the test user) would fail with "permission denied".
	runArgs := append([]string{"run", "-d"}, dockerPlatformArgs()...)
	runArgs = append(runArgs,
		"--name", containerName,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", datadir+":/data",
		"-p", fmt.Sprintf("127.0.0.1:%d:8545", hostPort),
		gethImageRef(),
		"--datadir", "/data",
		"--db.engine", "pebble",
		"--networkid", "1337",
		"--dev",
		"--dev.period", "1",
		"--dev.gaslimit", "60000000",
		"--http",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth,net,web3,txpool",
		"--http.corsdomain", "*",
		"--http.vhosts", "*",
		"--verbosity", "3",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("geth container started: %s", strings.TrimSpace(string(runOut)))

	// Capture logs in cleanup so a failure produces actionable output even
	// if geth exits before the test reaches its assertions.
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("geth container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()    //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	rpcURL := fmt.Sprintf("http://127.0.0.1:%d", hostPort)
	t.Logf("geth JSON-RPC: %s", rpcURL)

	// Geth typically takes 5–10 s to come up in passive mode; allow 60 s.
	if err := rpcprobe.WaitForRPC(rpcURL, 60*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup):\nerr: %v", err)
	}

	// ---- Phase 3: capture genesis state-root → $RESULT_PATH ----
	genesisRoot, err := rpcprobe.GenesisStateRoot(rpcURL)
	if err != nil {
		t.Fatalf("GenesisStateRoot: %v", err)
	}
	t.Logf("genesis stateRoot: %s", genesisRoot.Hex())
	result := oracle.SuiteResult{
		ClientName:       "geth",
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
