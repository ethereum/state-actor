//go:build cgo_nimbus && oracle

package nimbus_test

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

	clientnimbus "github.com/ethereum/state-actor/client/nimbus"
	"github.com/ethereum/state-actor/generator"
	stategenesis "github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
	e2e "github.com/ethereum/state-actor/internal/e2e_testing"
	"github.com/ethereum/state-actor/internal/engineapi"
	"github.com/ethereum/state-actor/internal/oracle"
	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// pinnedNimbusImage is the upstream nimbus-eth1 image the e2e suite boots.
// Override with NIMBUS_IMAGE=<ref>.
//
// v0.4.3 (commit 6e4d83c1) is a release that boots an Amsterdam genesis
// (Sepolia spec; v0.4.2 scheduled the Sepolia activation). It contains commit
// 2f0ae87cd ("Storage trie static vids (#4797)"), the last commit that changed
// the Aristo on-disk format (static storage-trie vertex IDs, the AccLeaf
// stoHint byte, the 0x7e SavedState trailer). internal/nimbus's codec and
// internal/nimbus/testdata/genesis_dump.json come from 2f0ae87cd; v0.4.3
// does not change that format. Move the pin and the fixture together.
const pinnedNimbusImage = "statusim/nimbus-eth1:v0.4.3"

func nimbusImageRef() string {
	if v := os.Getenv("NIMBUS_IMAGE"); v != "" {
		return v
	}
	return pinnedNimbusImage
}

const jwtSecretFileName = "jwt.hex"

// writeJWTSecret writes a random 32-byte secret as bare hex to
// <datadir>/jwt.hex — the format nimbus's --jwt-secret reads and the engine
// driver signs with.
func writeJWTSecret(datadir string) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("jwt secret: rand: %w", err)
	}
	path := filepath.Join(datadir, jwtSecretFileName)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret[:])+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("jwt secret: write %s: %w", path, err)
	}
	return path, nil
}

// TestE2ESuite — per-PR end-to-end gate for the nimbus writer + boot path.
// Phase 1-2 (datadir + nimbus.Run + boot the nimbus container) is
// nimbus-specific; Phase 3-7 run through internal/e2e.RunSuitePhases like
// every other client.
//
// Build-tagged `cgo_nimbus && oracle`; run via the cgo-suite CI job.
//
// Boot flags: `executionClient` selects the EL inside the combined `nimbus`
// binary; --network points at the empty-alloc sidecar and
// --debug-rewrite-datadir-id skips nimbus's rebuild-genesis-and-compare check
// (the analogue of ethrex's --skip-genesis-validation); --rpc-api accepts
// only eth/debug/admin; the engine API mandates a JWT; --max-peers=0 /
// --discv5=false / --nat=none keep the node off the network.
func TestE2ESuite(t *testing.T) {
	runE2ESuite(t, "osaka", engineapi.ForkOsaka)
}

// TestE2ESuiteAmsterdam runs the same suite on an Amsterdam genesis (the
// Sepolia spec). On top of the Osaka suite it proves that nimbus loads the
// Amsterdam header fields (blockAccessListHash, slotNumber) and the
// Amsterdam system contracts, then builds and imports Amsterdam blocks
// through engine_forkchoiceUpdatedV4 / getPayloadV6 / newPayloadV5.
func TestE2ESuiteAmsterdam(t *testing.T) {
	// The cross-client aggregator compares nimbus-result.json with the
	// Osaka roots of the other clients. The Amsterdam system contracts
	// change the root, so this run must not write that file (CI's
	// `-run TestE2ESuite` filter matches both tests).
	t.Setenv("RESULT_PATH", "")
	runE2ESuite(t, "amsterdam", engineapi.ForkAmsterdam)
}

func runE2ESuite(t *testing.T, fork string, engineFork engineapi.Fork) {
	if testing.Short() {
		t.Skip("e2e suite skipped in short mode")
	}
	const (
		seed             = int64(42)
		e2eBudget uint64 = 100 << 20
	)

	g, err := stategenesis.BuildSynthetic(fork, big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dd, cleanup := e2e.AcquireDatadir(t, "NIMBUS")
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
	specDoc, preAlloc := e2e.LoadCISpec(t, "../../examples/full-matrix-spec-feature.yaml", "nimbus")
	cfg.PreAlloc = preAlloc
	syscontracts.AddCanonicalSystemContracts(&cfg)
	if stategenesis.ForkAtLeast(fork, "amsterdam") {
		syscontracts.AddAmsterdamSystemContracts(&cfg)
	}

	if _, err := clientnimbus.Run(context.Background(), cfg, clientnimbus.Options{}); err != nil {
		t.Fatalf("nimbus.Run: %v", err)
	}

	jwtPath, err := writeJWTSecret(dd.HostPath)
	if err != nil {
		t.Fatalf("writeJWTSecret: %v", err)
	}
	// nimbus forces the data dir to 0700 at startup; the container runs as
	// root (see --user below) so that succeeds on a bind mount, but the dir
	// must be traversable.
	if err := os.Chmod(dd.HostPath, 0o777); err != nil {
		t.Fatalf("chmod datadir: %v", err)
	}

	eoas, contracts := oracle.Reproduce(oracle.ReproduceCfg{Seed: seed, AutoFill: plan})

	imageRef := nimbusImageRef()
	containerName := "state-actor-nimbus-boot-" + e2e.RandSuffix(8)
	runArgs := append([]string{"run", "-d"}, e2e.DockerPlatformArgs("NIMBUS_DOCKER_PLATFORM")...)
	runArgs = append(runArgs,
		"--name", containerName,
		// The release images run as an unprivileged "user". The writer
		// creates the datadir and jwt.hex as root, so nimbus must run as
		// root to open them (and to chmod the data dir).
		"--user", "0:0",
		"-v", dd.VolMount,
		imageRef,
		"executionClient",
		"--data-dir="+dd.ContainerDatadir,
		"--network="+dd.ContainerDatadir+"/"+clientnimbus.GenesisFileName,
		"--debug-rewrite-datadir-id",
		"--rpc", "--rpc-api=eth",
		"--http-address=0.0.0.0", "--http-port=8545",
		"--engine-api", "--engine-api-address=0.0.0.0", "--engine-api-port=8551",
		"--jwt-secret="+dd.ContainerDatadir+"/"+jwtSecretFileName,
		"--max-peers=0", "--discv5=false", "--nat=none",
	)
	runOut, err := exec.Command("docker", runArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %s\n%v", runOut, err)
	}
	t.Logf("nimbus container started: %s", strings.TrimSpace(string(runOut)))
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Logf("nimbus container logs:\n%s", logs)
		exec.Command("docker", "stop", containerName).Run()     //nolint:errcheck
		exec.Command("docker", "rm", "-f", containerName).Run() //nolint:errcheck
	})

	containerIP, err := e2e.InspectContainerIP(containerName)
	if err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("InspectContainerIP: %v\nnimbus logs:\n%s", err, logs)
	}
	rpcURL := "http://" + containerIP + ":8545"
	t.Logf("nimbus JSON-RPC: %s", rpcURL)

	if err := rpcprobe.WaitForRPC(rpcURL, 120*time.Second); err != nil {
		t.Fatalf("RPC never came up (logs captured in t.Cleanup): %v", err)
	}

	// A fully keyed trie boots without nimbus's computeKey walk; its presence
	// in the log means a branch shipped without its Merkle key.
	if logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput(); strings.Contains(string(logs), "computeKey cache") {
		t.Fatalf("nimbus rebuilt Merkle keys at boot — a branch lacks its stored key:\n%s", logs)
	}

	e2e.StartEngineDriverWithJWT(t, containerIP, rpcURL, engineFork, jwtPath)

	e2e.RunSuitePhases(t, e2e.SuitePhasesCfg{
		ClientName:      "nimbus",
		RPCURL:          rpcURL,
		EOAs:            eoas,
		Contracts:       contracts,
		GeneratorConfig: &cfg,
		Spec:            specDoc,
		SpecSeed:        e2e.CISpecSeed,
	})
}
