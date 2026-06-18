//go:build cgo_ethrex && oracle

package ethrex_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/state-actor/client/ethrex"
	"github.com/ethereum/state-actor/generator"
	stategenesis "github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
	e2e "github.com/ethereum/state-actor/internal/e2e_testing"
	"github.com/ethereum/state-actor/internal/engineapi"
	"github.com/ethereum/state-actor/internal/oracle"
	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// pinnedEthrexImage is the upstream ethrex Docker image the e2e suite pins
// against, digest-pinned for reproducibility. Override with
// ETHREX_IMAGE=ghcr.io/lambdaclass/ethrex:<tag> to test a specific release.
//
// Official release v16.0.0 (ghcr tag 16.0.0, published 2026-06-08), the first
// stable release >15.0.0 to include --skip-genesis-validation
// (lambdaclass/ethrex#6783, merged 2026-06-04), which the boot phase requires.
const pinnedEthrexImage = "ghcr.io/lambdaclass/ethrex:16.0.0@sha256:1873c36dcda955df9e5209b56e6fe47db67fb26abf00506de7695ba4683a1c5a"

func ethrexImageRef() string {
	if v := os.Getenv("ETHREX_IMAGE"); v != "" {
		return v
	}
	return pinnedEthrexImage
}

// jwtSecretFileName is the filename written into the datadir and mounted into
// the ethrex container for authrpc authentication.
const jwtSecretFileName = "jwt.hex"

// writeJWTSecret generates a random 32-byte JWT secret, writes it as a
// 64-character hex string (no 0x prefix) to <datadir>/jwt.hex, and returns
// the host-side path. ethrex reads --authrpc.jwtsecret on startup; the engine
// driver reads the same file to sign Bearer tokens.
func writeJWTSecret(datadir string) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("jwt secret: rand: %w", err)
	}
	hexStr := hex.EncodeToString(secret[:])
	path := filepath.Join(datadir, jwtSecretFileName)
	if err := os.WriteFile(path, []byte(hexStr+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("jwt secret: write %s: %w", path, err)
	}
	return path, nil
}

// TestE2ESuite — per-PR end-to-end gate for the ethrex writer + boot path.
// Phase 1-2 (datadir + ethrex.Run + boot ethrex container) is ethrex-specific;
// Phase 3-7 (capture root → re-query → spamoor → re-query → write result) is
// in internal/e2e.RunSuitePhases — same shape across all clients.
//
// Build-tagged `cgo_ethrex && oracle`. Run via the cgo-suite CI job;
// not in plain `go test ./...`. Spamoor binary must be on PATH or
// $SPAMOOR set; absent → t.Skip (or t.Fatal in CI when REQUIRE_SPAMOOR=1).
//
// JWT wiring: ethrex mandates authrpc JWT authentication and has no
// --engine-jwt-disabled equivalent. This test generates a random 32-byte
// secret, writes it to <datadir>/jwt.hex, mounts it into the container at
// the same container-relative path, and passes --authrpc.jwtsecret to
// ethrex. StartEngineDriverWithJWT reads the same file and signs every
// engine-API call with an HS256 Bearer token.
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

	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "ETHREX")
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

	specDoc, preAlloc := e2e.LoadCISpec(t, "../../examples/full-matrix-spec-feature.yaml", "ethrex")
	cfg.PreAlloc = preAlloc
	syscontracts.AddCanonicalSystemContracts(&cfg)

	if _, err := ethrex.Run(context.Background(), cfg, ethrex.Options{}); err != nil {
		t.Fatalf("ethrex.Run: %v", err)
	}

	// Generate a JWT secret for the authrpc endpoint. ethrex requires this;
	// there is no --engine-jwt-disabled flag. The same file is mounted into
	// the container and read by StartEngineDriverWithJWT so the engine driver
	// can sign Bearer tokens.
	jwtPath, err := writeJWTSecret(dd.HostPath)
	if err != nil {
		t.Fatalf("writeJWTSecret: %v", err)
	}
	containerJWTPath := dd.ContainerDatadir + "/" + jwtSecretFileName

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{
		Seed:     seed,
		AutoFill: plan,
	})

	imageRef := ethrexImageRef()
	containerName := "state-actor-ethrex-boot-" + e2e.RandSuffix(8)
	// ethrex reads --network to build genesis; --datadir for the RocksDB;
	// authrpc.jwtsecret for the engine-API JWT.
	networkPath := dd.ContainerDatadir + "/" + ethrex.GenesisFileName
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("ETHREX_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		"-v", dd.VolMount,
		imageRef,
		"--network", networkPath,
		"--datadir", dd.ContainerDatadir,
		// state-actor writes the state trie directly and ships a genesis sidecar
		// with an empty alloc, so ethrex must trust the stored state root rather
		// than recompute it from alloc. Requires lambdaclass/ethrex#6783.
		"--skip-genesis-validation",
		// Full sync, not the default snap. ethrex's engine fork_choice handler
		// (crates/networking/rpc/engine/fork_choice.rs) unconditionally returns
		// PayloadStatus::SYNCING with a null payloadId for EVERY forkchoiceUpdated
		// while sync_mode == Snap ("Ignore any FCU during snap-sync"), so the mock
		// CL can never get a payloadId to build on. With --syncmode full the FCU
		// reaches apply_fork_choice, which treats head==latest-canonical (our
		// genesis) as a normal build target and returns VALID + payloadId. There
		// are no peers, so neither mode actually syncs; full just doesn't gate.
		"--syncmode", "full",
		"--http.addr", "0.0.0.0",
		"--http.port", "8545",
		"--http.api", "eth,net,web3",
		"--authrpc.addr", "0.0.0.0",
		"--authrpc.port", "8551",
		"--authrpc.jwtsecret", containerJWTPath,
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("ethrex container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("ethrex container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()     //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nethrex logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("ethrex JSON-RPC: %s", rpcURL)

	if err := rpcprobe.WaitForRPC(rpcURL, 120*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup): %v", err)
	}

	// Drive block production via engine API. ethrex requires JWT on the
	// authrpc endpoint; StartEngineDriverWithJWT reads the secret from
	// jwtPath and signs every engine_forkchoiceUpdated / engine_newPayload
	// call with an HS256 Bearer token.
	e2e.StartEngineDriverWithJWT(t, containerIP, rpcURL, engineapi.ForkOsaka, jwtPath)

	e2e.RunSuitePhases(t, e2e.SuitePhasesCfg{
		ClientName:      "ethrex",
		RPCURL:          rpcURL,
		EOAs:            eoas,
		Contracts:       contracts,
		GeneratorConfig: &cfg,
		Spec:            specDoc,
		SpecSeed:        e2e.CISpecSeed,
	})
}
