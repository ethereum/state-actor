package e2e_testing

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/oracle"
	"github.com/nerolation/state-actor/internal/rpcprobe"
	"github.com/nerolation/state-actor/internal/spec"
)

// SuitePhasesCfg parameterizes the post-boot phases of a per-client e2e
// suite. The pre-boot phases (datadir setup + state-actor.Run + boot
// container + WaitForRPC) stay per-client — those are where the besu
// chainspec / neth boot.cfg / reth --dev / geth --dev knobs differ.
//
// Once the client's RPC is up, every per-client suite runs the SAME
// phases 3-7 against it: capture genesis stateRoot, pre-spamoor oracle
// re-query at "latest", spamoor for ~100 blocks, post-spamoor oracle
// re-query at "latest", write the final result.json. RunSuitePhases
// executes that sequence.
//
// Why both pre- and post-spamoor queries use "latest" (not "0x0"):
// geth's --dev mode runs PathDB in default full-gcmode, which prunes
// block 0's state once the chain advances past the ~128-block history
// window. With autofill-scale entity counts the pre-spamoor sweep
// takes longer than 128 sec at --dev.period=1, so any in-flight
// query at "0x0" trips "historical state not available" mid-iteration.
// Querying at "latest" sidesteps the limit and still verifies the
// genesis entities — spamoor's deployer is the only address that
// mutates state, so our random EOAs/contracts read identically at
// any tip.
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

	// GeneratorConfig is the generator.Config the writer was driven with.
	// RunSuitePhases reads InjectAddresses + GenesisAccounts/Code from it
	// during Phase 4 (via CheckInjections) to verify the writer didn't
	// silently drop any pre-allocated state — catches the class of bug
	// where a client writer ignores a cfg field, surfaces locally
	// instead of only via the cross-client genesis-root aggregator.
	GeneratorConfig *generator.Config

	// Spec is the parsed spec document — typically the same document
	// LoadCISpec returned. When non-nil, Phase 4 also calls
	// CheckERC20Templates against it (verifies every ERC-20 template's
	// name/symbol/decimals/totalSupply/balanceOf/allowance via eth_call).
	// SpecSeed must equal the build seed used to derive synthesized
	// random balances/allowances so the oracle re-derives the same values.
	Spec     *spec.Spec
	SpecSeed int64

	// SpamoorSlotDuration overrides the default 1s slot pacing — neth's
	// e2e historically used 250ms (matches smoke-nethermind-spamoor).
	// Zero → 1s default.
	SpamoorSlotDuration time.Duration

	// SkipBlockProduction returns from RunSuitePhases after Phase 4
	// (genesis re-query). Use when the client's block-production path
	// is known-broken AND tracked as a separate issue — the
	// cross-client genesis-root invariant gate (Phase 3) still runs
	// and result.json gets the partial GenesisStateRoot. Currently
	// set by nethermind because NethDev doesn't produce blocks on a
	// post-Prague chain with system contracts (issue #56).
	SkipBlockProduction bool
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

	// ---- Phase 3a: chainspec/header unification round-trip (#51) ----
	// Asserts the EL's RPC view of the genesis header matches the *Genesis
	// we wrote — catches any residual chainspec/header divergence (a
	// per-client writer hardcoding a literal would surface as a per-field
	// mismatch here, not as a confusing boot failure 20 lines later).
	if cfg.GeneratorConfig != nil && cfg.GeneratorConfig.Genesis != nil {
		AssertGenesisHeaderMatches(t, cfg.RPCURL, cfg.GeneratorConfig.Genesis)
	}

	// ---- Phase 4: pre-spamoor oracle re-query at "latest" ----
	// 4a — chain-level invariants: chainId + canonical syscontracts.
	// Cheap fail-fast for misconfigured genesis (besu chainspec drift,
	// missing syscontract injection, etc.) before walking entities.
	//
	// chainIDFromCfg returns 0 if any link in the nil-guard chain is
	// missing — we treat that as a caller bug (per-client test forgot
	// to populate Genesis.Config) instead of silently skipping the
	// check, since the chainspec-drift class of bug only surfaces here.
	chainID := chainIDFromCfg(cfg.GeneratorConfig)
	if chainID == 0 {
		t.Fatalf("cannot verify eth_chainId: cfg.GeneratorConfig.Genesis.Config.ChainID is nil; fix the per-client test setup")
	}
	if !CheckChainID(t, cfg.RPCURL, chainID) {
		t.Fatalf("genesis chain-id mismatch; aborting before spamoor phase")
	}
	if !CheckCanonicalSyscontracts(t, cfg.RPCURL, "latest") {
		t.Fatalf("canonical syscontracts missing; aborting before spamoor phase")
	}
	// 4b — entitygen synthetic entities (RNG-driven).
	if !CheckEntities(t, cfg.RPCURL, cfg.EOAs, cfg.Contracts, "latest") {
		t.Fatalf("pre-spamoor oracle re-query (entitygen) failed; aborting before spamoor phase")
	}
	// 4c — InjectAddresses (eth_getBalance > 0) + GenesisAccounts
	// (eth_getCode matches, explicit non-zero nonces match). Catches
	// writer-level drops of any field — see CheckInjections docstring.
	if !CheckInjections(t, cfg.RPCURL, cfg.GeneratorConfig, "latest") {
		t.Fatalf("pre-spamoor oracle re-query (injections + alloc) failed; aborting before spamoor phase")
	}
	// 4c′ — spec-entity storage, sampled straight from PreAlloc.
	// CheckInjections can't see it: GenesisStorage stays empty on the
	// spec path (Storage streams from cfg.PreAlloc to the writers), so
	// without this probe spec storage is gated only by cross-client root
	// agreement — identical wrong layouts on all four clients would pass.
	if cfg.GeneratorConfig != nil {
		if !CheckSpecStorage(t, cfg.RPCURL, cfg.GeneratorConfig.PreAlloc, "latest") {
			t.Fatalf("pre-spamoor oracle re-query (spec storage) failed; aborting before spamoor phase")
		}
	}
	// 4d — ERC-20 template oracle: name/symbol/decimals/totalSupply +
	// every explicit owner/allowance + sampled random ones. Verifies
	// the vendored OZ v5 bytecode is RPC-callable and every spec'd
	// field landed on every client.
	if cfg.Spec != nil {
		if !CheckERC20Templates(t, cfg.RPCURL, cfg.Spec, cfg.SpecSeed, "latest") {
			t.Fatalf("pre-spamoor oracle re-query (erc20 templates) failed; aborting before spamoor phase")
		}
	}
	if cfg.SkipBlockProduction {
		// Partial result.json already written in Phase 3. The
		// cross-client genesis-root aggregator job uses that.
		t.Logf("Phase 5-7 skipped (SkipBlockProduction=true)")
		return
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
		PrivKey:          oracle.SpamoorSenderPrivKey,
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
	deployerNonce, err := rpcprobe.EthGetTransactionCount(cfg.RPCURL, oracle.SpamoorSenderAddr, "latest")
	if err != nil {
		t.Errorf("eth_getTransactionCount %s: %v", oracle.SpamoorSenderAddr.Hex(), err)
	} else {
		result.PostSpamoorDeployerNonce = deployerNonce
		if deployerNonce == 0 {
			t.Errorf("post-spamoor deployer nonce is 0 — spamoor didn't send any txs?")
		}
	}

	// ---- Phase 6a: spamoor output verification ----
	// Phase 6 above proves spamoor's deployer SENT txs (nonce ticked) but
	// not that the client actually EXECUTED them. AssertSpamoorOutputs
	// finds spamoor's contract-creation tx in early blocks and asserts
	// the receipt is non-revert AND the deployed contract has code on
	// chain — closes the "txs landed in blocks but EVM silently dropped
	// the work" hole.
	AssertSpamoorOutputs(t, cfg.RPCURL)

	// ---- Phase 6b: chain-level post-spamoor invariants ----
	// Cheap supplementary gates: block-number > 0 (chain advanced) and
	// EIP-4788 BEACON_ROOTS ring buffer non-zero at the latest slot
	// (the pre-exec actually ran). Both surface client-side regressions
	// in block production / system-call wiring that the per-entity
	// oracle wouldn't catch.
	//
	// Bool returns land on SuiteResult so a pruned test-log doesn't hide
	// failures from the cross-client aggregator. Short-circuit BEACON_ROOTS
	// when chain didn't advance — calling BlockByNumber("latest") against
	// a stuck-at-zero chain produces a second, redundant error line.
	result.PostSpamoorChainAdvanced = CheckChainAdvanced(t, cfg.RPCURL)
	if result.PostSpamoorChainAdvanced {
		result.PostSpamoorBeaconRootsOK = CheckBeaconRootsRingBuffer(t, cfg.RPCURL)
	}

	// ---- Phase 7: write final result JSON ----
	if err := WriteResult(result); err != nil {
		t.Fatalf("WriteResult (post-spamoor): %v", err)
	}
}

// chainIDFromCfg walks cfg.Genesis.Config.ChainID and returns 0 on any
// nil link. Callers should treat 0 as "missing data — fix the caller",
// not as a valid chain-id (every supported chain-id is ≥ 1).
func chainIDFromCfg(cfg *generator.Config) uint64 {
	if cfg == nil || cfg.Genesis == nil ||
		cfg.Genesis.Config == nil || cfg.Genesis.Config.ChainID == nil {
		return 0
	}
	return cfg.Genesis.Config.ChainID.Uint64()
}
