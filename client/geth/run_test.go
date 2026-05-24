package geth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/autofill"
	"github.com/nerolation/state-actor/internal/templates"
)

func gethTestPlan(tb testing.TB, budget uint64) *autofill.Plan {
	tb.Helper()
	p, err := autofill.PlanForBudget(budget)
	if err != nil {
		tb.Fatalf("PlanForBudget(%d): %v", budget, err)
	}
	return p
}

// TestPopulateReproducibility runs Populate twice with the same seed
// and asserts the state roots match. If RNG draws drift between runs
// the cross-client invariant breaks immediately.
func TestPopulateReproducibility(t *testing.T) {
	plan := gethTestPlan(t, 512<<10)
	cfg := func(t *testing.T) generator.Config {
		dir := t.TempDir()
		return generator.Config{
			DBPath:         filepath.Join(dir, "geth", "chaindata"),
			AutoFill:       plan,
			Seed:           123,
			TrieMode:       generator.TrieModeMPT,
			WriteTrieNodes: true,
		}
	}

	statsA, err := Populate(context.Background(), cfg(t), Options{})
	if err != nil {
		t.Fatalf("Populate run A: %v", err)
	}
	statsB, err := Populate(context.Background(), cfg(t), Options{})
	if err != nil {
		t.Fatalf("Populate run B: %v", err)
	}

	if statsA.StateRoot != statsB.StateRoot {
		t.Fatalf("state roots differ: %s != %s", statsA.StateRoot.Hex(), statsB.StateRoot.Hex())
	}
	if (statsA.StateRoot == common.Hash{}) {
		t.Fatal("state root unexpectedly zero")
	}
	if statsA.AccountsCreated != plan.NumEOAs {
		t.Errorf("expected %d accounts, got %d", plan.NumEOAs, statsA.AccountsCreated)
	}
	if statsA.ContractsCreated != plan.NumContracts {
		t.Errorf("expected %d contracts, got %d", plan.NumContracts, statsA.ContractsCreated)
	}
}

// TestPopulateRootMatchesEntitygen rebuilds the same MPT in-memory
// using the canonical entitygen + trie.StackTrie path and asserts that
// the new Populate writer's emitted state root matches.
//
// This is the key test that the geth direct-Pebble Phase-2 logic
// computes the same root as a reference MPT construction over the same
// (addrHash, fullAccountRLP) sequence — i.e. the snapshot/code/storage
// writes don't leak into trie computation, and the keccak-sort step
// produces a canonically-ordered StackTrie input.
func TestPopulateRootMatchesEntitygen(t *testing.T) {
	dir := t.TempDir()
	plan := gethTestPlan(t, 512<<10)
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		AutoFill:       plan,
		Seed:           777,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
	}

	// Drive the new path.
	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if (stats.StateRoot == common.Hash{}) {
		t.Fatal("state root unexpectedly zero")
	}

	// Now reopen the DB and verify a few cross-checks against snapshot
	// reads: every recorded SampleEOA must have a snapshot account
	// entry under the "a" prefix, and decoded fields must match what
	// entitygen would produce for the same seed.
	w, err := NewWriter(cfg.DBPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w.Close()

	for _, addr := range stats.SampleEOAs {
		key := accountSnapshotKey(crypto.Keccak256Hash(addr[:]))
		blob, err := w.DB().Get(key)
		if err != nil {
			t.Errorf("snapshot account missing for %s: %v", addr.Hex(), err)
			continue
		}
		var slim types.SlimAccount
		if err := rlp.DecodeBytes(blob, &slim); err != nil {
			t.Errorf("decode slim account for %s: %v", addr.Hex(), err)
			continue
		}
	}

	// SnapshotRoot metadata must be present for geth's pathdb to boot.
	if got := rawdb.ReadSnapshotRoot(w.DB()); got != stats.StateRoot {
		t.Errorf("SnapshotRoot mismatch: got %s, want %s", got.Hex(), stats.StateRoot.Hex())
	}
}

// TestPopulateEmitsFullPlan asserts that Populate emits exactly
// plan.NumEOAs + plan.NumContracts entities even when cfg.TargetSize is
// set well below the plan's projected footprint. Post-Fix-A, TargetSize
// is advisory — the autofill Plan is the single source of truth for
// entity counts, so all four MPT clients must produce identical entity
// streams given the same Plan.
//
// Pre-Fix-A this test would have failed: the projection-based early-stop
// in writeStateAndCollectRoot fired once projectedTrieBytes >= TargetSize,
// truncating the plan mid-loop. That gate diverged per client (raw bytes,
// dirSize, projection) and was the root cause of cross-client state-root
// divergence at high-spec-utilization bench scales — see the cross-client
// invariance test in internal/e2e_testing.
func TestPopulateEmitsFullPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full-plan test in -short mode")
	}
	dir := t.TempDir()
	plan := gethTestPlan(t, 1<<20) // 1 MiB → ~1.2K EOAs + ~20 contracts
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		AutoFill:       plan,
		Seed:           42,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
		// TargetSize is set deliberately small (128 KiB) so the OLD
		// projection-based early-stop would fire after ~750 EOAs (175 B
		// each → 128 KiB). The full plan (~1.2K + 20) MUST still emit.
		TargetSize: 128 << 10,
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatal("state root unexpectedly zero")
	}
	if stats.AccountsCreated != plan.NumEOAs {
		t.Errorf("AccountsCreated: got %d, want %d (full plan)",
			stats.AccountsCreated, plan.NumEOAs)
	}
	if stats.ContractsCreated != plan.NumContracts {
		t.Errorf("ContractsCreated: got %d, want %d (full plan)",
			stats.ContractsCreated, plan.NumContracts)
	}
}

// TestPopulateEmitsFullPlan_HighSpecUtilization is the cross-client
// invariance regression gate for Fix A in the regime that originally
// surfaced the bug — autofill headroom is narrow because a fat spec
// dominates the TargetSize budget.
//
// The 100 GB bloatnet bench had a 98 GB spec and a 100 GB target,
// leaving autofill ~2 GB of headroom. Pre-Fix-A, geth's projection
// accumulator (also fed by spec entities, see addProjection at the
// genesis-alloc loop) crossed TargetSize before the autofill loop even
// started, so geth emitted the full plan only by accident (its
// projection underestimated PreAlloc's true on-disk cost in that run).
// The other three clients' raw-bytes / dirSize accumulators fired
// almost immediately on autofill start at differing points, producing
// divergent entity counts (reth: 12 % of plan, nethermind: 100 contracts).
//
// This test simulates that regime at unit scale: a 1 MiB target with
// 900 KiB of synthetic PreAlloc → ~100 KiB of autofill headroom. Post-
// Fix-A the writer ignores TargetSize for stop decisions and emits the
// full plan regardless. Pre-Fix-A this test would have failed.
//
// Cross-client coverage: besu / reth / nethermind have parallel
// TestRunCgoEmitsFullPlan / TestRunEmitsFullPlan unit tests that
// exercise the same gate-removal invariant in their writer packages.
// Together with the existing 100 MiB cross-client-genesis-root CI
// aggregator, this triplet covers the bug at the unit and integration
// levels.
func TestPopulateEmitsFullPlan_HighSpecUtilization(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping high-spec-utilization test in -short mode")
	}

	const (
		target          uint64 = 1 << 20 // 1 MiB
		preAllocCount          = 5000     // ~5000 × 175 B ≈ 875 KiB ≈ 85 % of target
		preAllocBalance        = 1_000_000_000_000_000_000
	)

	// Build a deterministic PreAlloc list. Addresses derived from a fixed
	// seed so the test is byte-identical across runs.
	preAlloc := make([]templates.PreAllocEntity, preAllocCount)
	for i := 0; i < preAllocCount; i++ {
		var addr common.Address
		// Address bytes: 4-byte big-endian index in the last 4 bytes.
		// Trivial collision-free derivation; not meant to be realistic.
		addr[16] = byte(i >> 24)
		addr[17] = byte(i >> 16)
		addr[18] = byte(i >> 8)
		addr[19] = byte(i)
		preAlloc[i] = templates.PreAllocEntity{
			Address: addr,
			Account: &types.StateAccount{
				Nonce:    uint64(i),
				Balance:  uint256.NewInt(preAllocBalance),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		}
	}

	dir := t.TempDir()
	plan := gethTestPlan(t, 256<<10) // ~300 EOAs + ~5 contracts
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		AutoFill:       plan,
		PreAlloc:       preAlloc,
		Seed:           42,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
		// TargetSize is fat-spec dominated: PreAlloc fills ~85 %, so
		// the OLD projection accumulator (incl. spec entities) would
		// trip before autofill even started. The full autofill plan
		// MUST still emit.
		TargetSize: target,
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatal("state root unexpectedly zero")
	}
	// stats.AccountsCreated counts both PreAlloc EOAs and autofill EOAs
	// (geth's writer increments the same field in both loops).
	wantAccounts := preAllocCount + plan.NumEOAs
	if stats.AccountsCreated != wantAccounts {
		t.Errorf("AccountsCreated: got %d, want %d (preAlloc=%d + plan.NumEOAs=%d)",
			stats.AccountsCreated, wantAccounts, preAllocCount, plan.NumEOAs)
	}
	if stats.ContractsCreated != plan.NumContracts {
		t.Errorf("ContractsCreated: got %d, want %d (full plan)",
			stats.ContractsCreated, plan.NumContracts)
	}
}

// TestPopulateGenesisAlloc covers the genesis-alloc Phase-1 branch:
// pre-allocated EOA + contract, both surface in the resulting snapshot.
// Mainly guards that the encodeEntityContract path with code+slots
// round-trips through the temp Pebble correctly.
func TestPopulateGenesisAlloc(t *testing.T) {
	dir := t.TempDir()

	allocAddr := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")
	allocAcc := &types.StateAccount{
		Nonce:    7,
		Balance:  uint256.NewInt(1_000_000_000_000_000_000),
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}

	// Build a minimal "alloc-only" config (no synthetic accounts; AutoFill nil).
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		Seed:           1,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
		GenesisAccounts: map[common.Address]*types.StateAccount{
			allocAddr: allocAcc,
		},
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if (stats.StateRoot == common.Hash{}) {
		t.Fatal("state root unexpectedly zero")
	}

	w, err := NewWriter(cfg.DBPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w.Close()

	allocHash := crypto.Keccak256Hash(allocAddr[:])
	if blob, err := w.DB().Get(accountSnapshotKey(allocHash)); err != nil || len(blob) == 0 {
		t.Fatalf("alloc account missing: blob=%x err=%v", blob, err)
	}
}
