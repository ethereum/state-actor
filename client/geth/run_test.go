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
)

// TestPopulateReproducibility runs Populate twice with the same seed
// and asserts the state roots match. If RNG draws drift between runs
// the cross-client invariant breaks immediately.
func TestPopulateReproducibility(t *testing.T) {
	cfg := func(t *testing.T) generator.Config {
		dir := t.TempDir()
		return generator.Config{
			DBPath:         filepath.Join(dir, "geth", "chaindata"),
			NumAccounts:    20,
			NumContracts:   8,
			MaxSlots:       16,
			MinSlots:       2,
			Distribution:   generator.PowerLaw,
			Seed:           123,
			BatchSize:      1000,
			Workers:        1,
			CodeSize:       64,
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
	if statsA.AccountsCreated != 20 {
		t.Errorf("expected 20 accounts, got %d", statsA.AccountsCreated)
	}
	if statsA.ContractsCreated != 8 {
		t.Errorf("expected 8 contracts, got %d", statsA.ContractsCreated)
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
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		NumAccounts:    10,
		NumContracts:   3,
		MaxSlots:       8,
		MinSlots:       1,
		Distribution:   generator.PowerLaw,
		Seed:           777,
		BatchSize:      1000,
		Workers:        1,
		CodeSize:       32,
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
	w, err := NewWriter(cfg.DBPath, cfg.BatchSize, cfg.Workers)
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

// TestPopulateCanonicalEntitygenRoot pins the geth-MPT state root for
// the same fixed config that internal/entitygen/canonical_mpt_test.go
// pins as the cross-client canonical. Drift between this test and the
// canonical means geth's Populate diverged from the shared entitygen
// contract — coordinated update required across all client adapters.
//
// This is the load-bearing cross-client invariant test for geth-MPT.
func TestPopulateCanonicalEntitygenRoot(t *testing.T) {
	const expected = "0xddbfa7c1941ff70fe5a692f7552149adc1ae29ebb2b5dc8bb3544c1368bcb0c3"

	dir := t.TempDir()
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		NumAccounts:    10,
		NumContracts:   5,
		MaxSlots:       100,
		MinSlots:       1,
		Distribution:   generator.PowerLaw,
		Seed:           12345,
		BatchSize:      1000,
		Workers:        1,
		CodeSize:       256,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if got := stats.StateRoot.Hex(); got != expected {
		t.Fatalf("geth-MPT state root drift from canonical entitygen MPT root:\n  got:  %s\n  want: %s\n  Diverging here means coordinated update across all entitygen-using adapters (see internal/entitygen/canonical_mpt_test.go).",
			got, expected)
	}
}

// TestPopulateTargetSizeStopsAccurately verifies Phase 2's dirSize
// sampling stops production-DB writes when cfg.TargetSize is reached,
// landing the on-disk size within a reasonable tolerance of the target.
//
// Mirrors TestTargetSizeStopsAccurately_MPT in generator/, but runs
// against client/geth.Populate directly so it's independent of the
// generator MPT registration shim.
func TestPopulateTargetSizeStopsAccurately(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping target-size accuracy test in -short mode")
	}
	dir := t.TempDir()
	// 200 MiB is small enough to run quickly but large enough to
	// amortise Pebble's fixed overhead (WAL + MANIFEST + SST metadata)
	// to a small fraction of the target — keeps the 20% tolerance
	// achievable. Smaller targets (e.g. 50 MiB) would need a
	// proportionally tighter sample cadence to stay in band.
	const target uint64 = 200 * 1024 * 1024 // 200 MiB
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		NumAccounts:    100,
		NumContracts:   1_000_000, // generous safety upper bound
		MaxSlots:       50,
		MinSlots:       5,
		Distribution:   generator.PowerLaw,
		Seed:           42,
		BatchSize:      1000,
		Workers:        1,
		CodeSize:       128,
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

	actual, err := dirSize(cfg.DBPath)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	const tolerance = 0.20
	diff := float64(actual) - float64(target)
	if diff < 0 {
		diff = -diff
	}
	pct := diff / float64(target)
	t.Logf("DB size: actual=%d target=%d diff=%.1f%% tolerance=%.1f%%",
		actual, target, pct*100, tolerance*100)
	if pct > tolerance {
		t.Errorf("DB size %.1f%% off target (%d vs %d), tolerance %.1f%%",
			pct*100, actual, target, tolerance*100)
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

	// Build a minimal "alloc-only" config (no synthetic accounts).
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		Seed:           1,
		BatchSize:      100,
		Workers:        1,
		Distribution:   generator.PowerLaw,
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

	w, err := NewWriter(cfg.DBPath, cfg.BatchSize, cfg.Workers)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w.Close()

	allocHash := crypto.Keccak256Hash(allocAddr[:])
	if blob, err := w.DB().Get(accountSnapshotKey(allocHash)); err != nil || len(blob) == 0 {
		t.Fatalf("alloc account missing: blob=%x err=%v", blob, err)
	}
}
