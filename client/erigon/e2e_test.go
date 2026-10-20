//go:build cgo_erigon && oracle

package erigon_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/state-actor/client/erigon"
	"github.com/ethereum/state-actor/generator"
	stategenesis "github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
	e2e "github.com/ethereum/state-actor/internal/e2e_testing"
	"github.com/ethereum/state-actor/internal/engineapi"
	"github.com/ethereum/state-actor/internal/genesisheader"
	"github.com/ethereum/state-actor/internal/oracle"
	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// erigonImageRef is the image the boot phase runs as the erigon daemon.
// Defaults to the cgo-suite builder image this test runs inside: it ships
// the locally-built erigon at /usr/local/bin/erigon — the exact pinned
// commit (546790a6a7) state-actor's commitment vendor depends on. Override
// with ERIGON_IMAGE for local runs against a different tag.
func erigonImageRef() string {
	if v := os.Getenv("ERIGON_IMAGE"); v != "" {
		return v
	}
	return "state-actor-erigon-builder:latest"
}

const erigonJWTFileName = "jwt.hex"

// writeErigonJWTSecret generates a random 32-byte JWT secret, writes it as
// a 64-char hex string to <datadir>/jwt.hex, and returns the host path.
// erigon mandates authrpc JWT (it has no --authrpc.jwt-disabled flag); the
// engine driver reads the same file to sign Bearer tokens.
func writeErigonJWTSecret(datadir string) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("jwt secret: rand: %w", err)
	}
	path := filepath.Join(datadir, erigonJWTFileName)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret[:])+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("jwt secret: write %s: %w", path, err)
	}
	return path, nil
}

// TestE2ESuite — per-PR end-to-end gate for the erigon writer + boot path.
// Phase 1-2 (datadir + erigon.Run + boot the erigon daemon) is erigon-
// specific; Phase 3-7 (capture root → re-query → spamoor → re-query →
// write result) is e2e.RunSuitePhases, the same shape across all clients.
//
// Build-tagged `cgo_erigon && oracle`; run via the cgo-suite CI job, not
// plain `go test ./...`. Spamoor binary must be on PATH or $SPAMOOR set;
// absent → t.Skip (or t.Fatal in CI when REQUIRE_SPAMOOR=1).
//
// erigon boot specifics (vs besu/geth): the daemon needs --externalcl
// (v3.4.2's --chain dev is broken), --snap.stop + --snap.state.stop (else
// StageSnapshots gates engine_forkchoiceUpdated to SYNCING forever), and a
// mandatory authrpc JWT. The writer (run_cgo.go) seeds the SyncStage
// markers + the fat-genesis MaxTxNum so block production advances past
// block 2.
func TestE2ESuite(t *testing.T) {
	runE2ESuite(t, "osaka")
}

// TestE2ESuiteAmsterdam runs the same suite on an Amsterdam genesis
// (Sepolia spec). On top of the Osaka suite it proves that `erigon init`
// writes the Amsterdam header fields (blockAccessListHash, slotNumber),
// that the stateRoot patch keeps them, and that erigon builds and imports
// Amsterdam blocks on top of the Amsterdam system contracts.
func TestE2ESuiteAmsterdam(t *testing.T) {
	// The cross-client aggregator compares erigon-result.json with the
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
		seed             = int64(42)
		e2eBudget uint64 = 100 << 20 // ~100 MB synthetic trie state
	)

	g, err := stategenesis.BuildSynthetic(fork, big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "ERIGON")
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

	specDoc, preAlloc := e2e.LoadCISpec(t, "../../examples/full-matrix-spec-feature.yaml", "erigon")
	cfg.PreAlloc = preAlloc
	syscontracts.AddCanonicalSystemContracts(&cfg)
	engineFork := engineapi.ForkOsaka
	if stategenesis.ForkAtLeast(fork, "amsterdam") {
		syscontracts.AddAmsterdamSystemContracts(&cfg)
		engineFork = engineapi.ForkAmsterdam
	}

	if _, err := erigon.Run(context.Background(), cfg, erigon.Options{}); err != nil {
		t.Fatalf("erigon.Run: %v", err)
	}

	// erigon requires authrpc JWT; the same file is mounted into the
	// container and read by StartEngineDriverWithJWT to sign Bearer tokens.
	jwtPath, err := writeErigonJWTSecret(dd.HostPath)
	if err != nil {
		t.Fatalf("writeErigonJWTSecret: %v", err)
	}
	containerJWTPath := dd.ContainerDatadir + "/" + erigonJWTFileName

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:     seed,
		AutoFill: plan,
	})

	imageRef := erigonImageRef()
	containerName := "state-actor-erigon-boot-" + e2e.RandSuffix(8)
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("ERIGON_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		// The builder image has no ENTRYPOINT; boot the locally-built erigon.
		"--entrypoint", "/usr/local/bin/erigon",
		imageRef,
		"--datadir", dd.ContainerDatadir,
		"--networkid", "1337",
		"--no-downloader",   // no snapshot peer download
		"--snap.stop",       // don't gate boot on StageSnapshots
		"--snap.state.stop", // ditto for the state snapshots
		"--externalcl",      // drive blocks via the engine API (no --chain dev)
		"--authrpc.addr", "0.0.0.0",
		"--authrpc.port", "8551",
		"--authrpc.jwtsecret", containerJWTPath,
		"--port", "0", // random p2p port; no inbound peers
		"--http", "--http.addr=0.0.0.0", "--http.port=8545",
		"--http.api=eth,net,web3,txpool,debug",
		"--nodiscover",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("erigon container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("erigon container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()     //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nerigon logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("erigon JSON-RPC: %s", rpcURL)

	// erigon v3.4.2 boot (chaindata open + snapshot stage init) is slower
	// than geth; allow 180s.
	if err := rpcprobe.WaitForRPC(rpcURL, 180*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup): %v", err)
	}

	// erigon init builds the genesis header and the writer patches only
	// its stateRoot, so check the header before the chain advances.
	assertGenesisHeaderHash(t, rpcURL, g)

	// Drive block production via the engine API with JWT-signed calls.
	e2e.StartEngineDriverWithJWT(t, containerIP, rpcURL, engineFork, jwtPath)

	e2e.RunSuitePhases(t, e2e.SuitePhasesCfg{
		ClientName:      "erigon",
		RPCURL:          rpcURL,
		EOAs:            eoas,
		Contracts:       contracts,
		GeneratorConfig: &cfg,
		Spec:            specDoc,
		SpecSeed:        e2e.CISpecSeed,
	})
}

// assertGenesisHeaderHash checks that erigon's genesis block hash equals
// the hash of genesisheader.Build for the same genesis and stateRoot.
// genesisheader.Build is pinned to `geth init` output, so a match proves
// that erigon serves the same genesis block as the other clients. For
// Amsterdam it also checks the two header fields that the fork adds.
func assertGenesisHeaderHash(t *testing.T, rpcURL string, g *stategenesis.Genesis) {
	t.Helper()
	raw, err := rpcprobe.Call(rpcURL, "eth_getBlockByNumber", []any{"0x0", false})
	if err != nil {
		t.Fatalf("eth_getBlockByNumber(0x0): %v", err)
	}
	var block struct {
		Hash                common.Hash     `json:"hash"`
		StateRoot           common.Hash     `json:"stateRoot"`
		BlockAccessListHash *common.Hash    `json:"blockAccessListHash"`
		SlotNumber          *hexutil.Uint64 `json:"slotNumber"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("decode genesis block: %v (raw: %s)", err, raw)
	}
	want := genesisheader.Build(g, 0, common.Hash{}, block.StateRoot).Hash()
	if block.Hash != want {
		t.Errorf("genesis hash = %s; want %s (genesisheader.Build)", block.Hash.Hex(), want.Hex())
	}
	if g.Config.AmsterdamTime == nil {
		return
	}
	if block.BlockAccessListHash == nil || *block.BlockAccessListHash != types.EmptyBlockAccessListHash {
		t.Errorf("genesis blockAccessListHash = %v; want %s", block.BlockAccessListHash, types.EmptyBlockAccessListHash.Hex())
	}
	if block.SlotNumber == nil || *block.SlotNumber != 0 {
		t.Errorf("genesis slotNumber = %v; want 0x0", block.SlotNumber)
	}
}
