package oracle

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// SuitePhasesCfg parameterizes the post-boot phases of a per-client e2e
// suite. The pre-boot phases (datadir setup + state-actor.Run + boot
// container + WaitForRPC) stay per-client — those are where the besu
// chainspec / neth boot.cfg / reth --dev / geth --dev knobs differ.
//
// Once the client's RPC is up, every per-client suite runs the SAME
// phases 3-7 against it: capture genesis stateRoot, oracle re-query at
// "0x0", spamoor for ~100 blocks, oracle re-query at "latest", write
// the final result.json. RunSuitePhases executes that sequence.
type SuitePhasesCfg struct {
	// ClientName is one of "besu" / "geth" / "nethermind" / "reth".
	// Goes into the SuiteResult JSON for the cross-client aggregator.
	ClientName string

	// RPCURL is the running client's JSON-RPC endpoint
	// (e.g. "http://172.17.0.4:8545" for DinD bridge mode or
	// "http://127.0.0.1:<port>" for host-port mapping).
	RPCURL string

	// EOAs / Contracts are the entitygen-replayed expected entities.
	// Caller built these via Reproduce(ReproduceCfg{...}) with the
	// same seed/counts the writer used.
	EOAs      []*entitygen.Account
	Contracts []*entitygen.Account

	// SpamoorSlotDuration overrides the default 1s slot pacing — neth's
	// e2e historically used 250ms (matches smoke-nethermind-spamoor).
	// Zero → 1s default.
	SpamoorSlotDuration time.Duration
}

// RunSuitePhases executes phases 3-7 of the per-client e2e suite. Each
// phase is a t.Fatalf on hard error so the caller gets GHA-natural
// fail-fast: phase N+1 doesn't run if N failed.
//
// Phase 3 (capture genesis state-root) writes a partial SuiteResult to
// $RESULT_PATH so the cross-client aggregator gets the genesis root
// even if a later phase fails.
//
// Phase 5 (spamoor) skips with t.Skipf if SPAMOOR isn't on PATH AND
// REQUIRE_SPAMOOR=1 is unset (CI sets it; local-dev doesn't). Avoids
// the "Docker silently mounts an empty directory at /usr/local/bin/
// spamoor" footgun in CI while keeping local dev ergonomic.
func RunSuitePhases(t *testing.T, cfg SuitePhasesCfg) {
	t.Helper()

	// ---- Phase 3: capture genesis state-root → $RESULT_PATH ----
	genesisRoot, err := rpcprobe.GenesisStateRoot(cfg.RPCURL)
	if err != nil {
		t.Fatalf("GenesisStateRoot: %v", err)
	}
	t.Logf("genesis stateRoot: %s", genesisRoot.Hex())
	result := SuiteResult{
		ClientName:       cfg.ClientName,
		GenesisStateRoot: genesisRoot,
	}
	if err := WriteResult(result); err != nil {
		t.Fatalf("WriteResult (pre-spamoor): %v", err)
	}

	// ---- Phase 4: oracle re-query at "0x0" ----
	if !CheckEntities(t, cfg.RPCURL, cfg.EOAs, cfg.Contracts, "0x0") {
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
	slotDur := cfg.SpamoorSlotDuration
	if slotDur == 0 {
		slotDur = time.Second
	}
	postBlock, err := SpamoorRun(SpamoorRunCfg{
		Binary:           spamoorBin,
		RPCURL:           cfg.RPCURL,
		PrivKey:          SpamoorSenderPrivKey,
		Seed:             12345,
		TargetBlockDelta: 100,
		SlotDuration:     slotDur,
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
	if CheckEntities(t, cfg.RPCURL, cfg.EOAs, cfg.Contracts, "latest") {
		result.PostSpamoorEntityCheck = "ok"
	} else {
		result.PostSpamoorEntityCheck = "entitygen entities drifted post-spamoor"
	}
	deployerNonce, err := rpcprobe.EthGetTransactionCount(cfg.RPCURL, SpamoorSenderAddr, "latest")
	if err != nil {
		t.Errorf("eth_getTransactionCount %s: %v", SpamoorSenderAddr.Hex(), err)
	} else {
		result.PostSpamoorDeployerNonce = deployerNonce
		if deployerNonce == 0 {
			t.Errorf("post-spamoor deployer nonce is 0 — spamoor didn't send any txs?")
		}
	}

	// ---- Phase 7: write final result JSON ----
	if err := WriteResult(result); err != nil {
		t.Fatalf("WriteResult (post-spamoor): %v", err)
	}
}
