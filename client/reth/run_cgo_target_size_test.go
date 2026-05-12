//go:build cgo_reth && large

package reth

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
)

// TestRunCgoTargetSizeStopsAccurately verifies the per-batch dirSize
// sampling in Phase 4b/4c stops production-DB writes (MDBX + RocksDB +
// static_files + sorter spill) when cfg.TargetSize is reached, landing
// the on-disk size within a reasonable tolerance of the target.
//
// Mirrors client/geth/run_test.go:TestPopulateTargetSizeStopsAccurately
// and client/nethermind/run_cgo_target_size_test.go.
//
// MDBX preallocation caveat: reth's mdbx.dat grows in coarse steps
// (default 4 GiB on first growth). On filesystems that report logical
// (apparent) size, dirSize can over-report and trip the cap earlier
// than the actual data warrants. If that surfaces here, switch the
// dirSize helper in run_cgo.go to sample only rocksdb/ + static_files/
// (excluding db/) and re-run. Until then, this test uses a 200 MiB
// target — large enough that even mdbx.dat's first growth-step
// shouldn't blow past the ±20% tolerance.
func TestRunCgoTargetSizeStopsAccurately(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping target-size accuracy test in -short mode")
	}
	dir := t.TempDir()

	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	const target uint64 = 200 * 1024 * 1024 // 200 MiB
	cfg := generator.Config{
		DBPath:       filepath.Join(dir, "reth-datadir"),
		NumAccounts:  100,
		NumContracts: 1_000_000, // generous safety upper bound
		MaxSlots:     50,
		MinSlots:     5,
		Distribution: generator.PowerLaw,
		Seed:         42,
		// Smaller-than-default batch so the per-batch dirSize sampling
		// fires often enough to land within ±20% of 200 MiB. At default
		// CodeSize=128 with 5-50 slots/contract the average production-
		// DB delta per contract is ~7.5 KiB (slot RLP + MPT trie node
		// overhead). 2000-batch ≈ 15 MiB → at most one batch (7.5% of
		// target) of overshoot. Production users running with 10 GiB+
		// targets at the default 100K-batch only see ~1% overshoot.
		CodeSize:   128,
		TrieMode:   generator.TrieModeMPT,
		Genesis:    g,
		TargetSize: target,
	}

	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
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
	t.Logf("reth DB size: actual=%d target=%d diff=%.1f%% tolerance=%.1f%%",
		actual, target, pct*100, tolerance*100)
	if pct > tolerance {
		t.Errorf("DB size %.1f%% off target (%d vs %d), tolerance %.1f%%",
			pct*100, actual, target, tolerance*100)
	}
}
