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

// TestE2ESuite — see client/besu/e2e_test.go for the full phase
// description. geth-specific bits:
//   - --fork=osaka (matches all 4 clients post-Pre-C-v2).
//   - No DinD; geth runs with -p host-port mapping.
//   - Phase 5-7 (spamoor + post-spamoor re-query) is SKIPPED for geth
//     because state-actor's chain is post-merge → block production
//     requires a CL (engine API) or pre-merge PoW with non-trivial
//     ethash setup. The cross-client aggregator still includes geth's
//     genesis stateRoot from Phase 3. Adding a beacon-mock for geth
//     spamoor is tracked as a follow-up.
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
	// InjectAddresses is set even though geth skips Phase 5-7 (spamoor)
	// — it has to match the alloc shape the other 3 clients write so the
	// cross-client genesis state-root invariant holds. Pre-funding 1
	// extra account adds 1 entry to the writer's account stream;
	// without this geth's stateRoot diverges from besu/neth/reth's by
	// exactly that one entry.
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
		InjectAddresses: []common.Address{common.HexToAddress("0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")},
	}
	if _, err := Populate(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Populate: %v", err)
	}

	// Reproduce the RNG sequence state-actor's geth Phase 1 used.
	// internal/oracle.Reproduce is the single source of truth across
	// all 4 per-client boot tests + e2e suites.
	//
	// We don't need state-actor's genesisAddrs collision-retry loop here:
	// this test passes no genesis alloc and no --inject-accounts, so the
	// map is empty and no re-rolls happen — matching Reproduce's
	// no-inject assumption.
	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:         seed,
		NumAccounts:  numAccounts,
		NumContracts: numContracts,
		CodeSize:     codeSize,
		MinSlots:     minSlots,
		MaxSlots:     maxSlots,
		Distribution: generator.PowerLaw,
	})

	// Boot upstream geth in passive read-only mode. --syncmode=full +
	// --nodiscover + --maxpeers=0 keep the node from peering or syncing;
	// --networkid=1337 matches the chain ID baked into the synthesized
	// genesis. --db.engine=pebble points geth at our Pebble datadir
	// (default is leveldb on geth ≤ v1.13).
	containerName := "state-actor-geth-boot-" + randSuffix(8)
	hostPort := freeTCPPort(t)

	// Run geth as the test process's UID/GID so any files it writes to
	// the bind-mounted /data stay owned by the test user. Without this,
	// on native Linux (e.g. GHA runners), geth's default-root container
	// would write root-owned files into datadir, and t.TempDir's cleanup
	// (running as the test user) would fail with "permission denied".
	// Docker Desktop for Mac masks this via filesystem-layer UID mapping;
	// the test still passed locally pre-fix.
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
		"--syncmode", "full",
		"--nodiscover",
		"--maxpeers", "0",
		"--http",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth,net,web3",
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
		// PostSpamoor* fields left zero — see TestE2ESuite docstring.
	}
	if err := oracle.WriteResult(result); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	// ---- Phase 4: oracle re-query at "0x0" ----
	// (Phases 5-7 — spamoor + post-spamoor re-query — skipped for geth;
	// see TestE2ESuite docstring.)
	for _, eoa := range eoas {
		got, err := rpcprobe.EthGetBalance(rpcURL, eoa.Address, "0x0")
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
	for _, c := range contracts {
		gotCode, err := rpcprobe.EthGetCode(rpcURL, c.Address, "0x0")
		if err != nil {
			t.Errorf("eth_getCode %s: %v", c.Address.Hex(), err)
		} else if !bytes.Equal(gotCode, c.Code) {
			t.Errorf("eth_getCode %s: len got=%d want=%d (first 32 bytes: got=%x want=%x)",
				c.Address.Hex(), len(gotCode), len(c.Code),
				safePrefix(gotCode, 32), safePrefix(c.Code, 32))
		}
		for _, slot := range c.Storage {
			got, err := rpcprobe.EthGetStorageAt(rpcURL, c.Address, slot.Key, "0x0")
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
