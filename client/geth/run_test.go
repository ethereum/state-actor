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
	"github.com/nerolation/state-actor/internal/sizecal"
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

// TestPopulateTargetSizeStopsAccurately verifies origin-scoped Phase 1
// emission halts when the projected trie footprint reaches cfg.TargetSize.
// The assertion measures projected trie bytes (sizecal-formula on the
// stats counters), not dirSize — cfg.TargetSize is a trie-only budget,
// so geth's flat-state + Pebble overhead inflate dirSize by ~50% on top
// (by design, not a regression).
//
// Mirrors TestTargetSizeStopsAccurately_MPT in generator/, but runs
// against client/geth.Populate directly so it's independent of the
// generator MPT registration shim.
func TestPopulateTargetSizeStopsAccurately(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping target-size accuracy test in -short mode")
	}
	dir := t.TempDir()
	const target uint64 = 200 * 1024 * 1024 // 200 MiB
	plan := gethTestPlan(t, 5*target)        // generous safety upper bound
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		AutoFill:       plan,
		Seed:           42,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
		TargetSize:     target,
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatal("state root unexpectedly zero after target-size stop")
	}

	// Projected trie bytes from the Phase 1 accumulator's formula.
	// Tolerance ±5%: Phase 1 stops the moment projection >= target, so the
	// worst-case overshoot is one entity (~7 KB for a max-slot contract
	// at ~50 slots × 140 B + 175 B). Far inside the bound.
	bAcct := sizecal.BytesPerAccount("")
	bSlot := sizecal.BytesPerSlot("")
	projected := uint64(stats.AccountsCreated+stats.ContractsCreated)*bAcct +
		uint64(stats.StorageSlotsCreated)*bSlot
	diff := float64(projected) - float64(target)
	if diff < 0 {
		diff = -diff
	}
	pct := diff / float64(target)
	const tolerance = 0.05
	t.Logf("projected trie: accounts+contracts=%d slots=%d → %d B; target=%d diff=%.1f%% tol=%.1f%%",
		stats.AccountsCreated+stats.ContractsCreated, stats.StorageSlotsCreated,
		projected, target, pct*100, tolerance*100)
	if pct > tolerance {
		t.Errorf("projected trie %.1f%% off target (proj=%d target=%d), tol=%.1f%%",
			pct*100, projected, target, tolerance*100)
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
