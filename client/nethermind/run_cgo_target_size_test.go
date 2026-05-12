//go:build cgo_neth

package nethermind

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
)

// TestRunTargetSizeStopsAccurately verifies the Phase 2 dirSize sampling
// in entitygen_cgo.go stops production-DB writes (State + Code RocksDBs)
// when cfg.TargetSize is reached, landing the on-disk size within a
// reasonable tolerance of the target.
//
// Mirrors client/geth/run_test.go:TestPopulateTargetSizeStopsAccurately —
// same target, same generous upper-bound count, same ±20% tolerance.
//
// NumContracts is set to a large safety upper bound so the Phase 1 5×
// raw-byte cap kicks in long before the loop completes — Phase 2's
// per-1024-entity dirSize stop then lands the directory size near target.
func TestRunTargetSizeStopsAccurately(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping target-size accuracy test in -short mode")
	}
	dir := t.TempDir()

	// Genesis is required: writeChainSpec + the writer pipeline need a
	// non-nil g.Config. Osaka matches the e2e suite's default fork.
	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	// 200 MiB matches geth's target. Smaller targets would need a
	// proportionally tighter sample cadence to stay in band; the 1024-
	// entity cadence in entitygen_cgo.go is calibrated for this size.
	const target uint64 = 200 * 1024 * 1024
	cfg := generator.Config{
		DBPath:       filepath.Join(dir, "neth"),
		NumAccounts:  100,
		NumContracts: 1_000_000, // generous safety upper bound
		MaxSlots:     50,
		MinSlots:     5,
		Distribution: generator.PowerLaw,
		Seed:         42,
		CodeSize:     128,
		TrieMode:     generator.TrieModeMPT,
		Genesis:      g,
		TargetSize:   target,
	}

	stats, err := Run(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
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
	t.Logf("nethermind DB size: actual=%d target=%d diff=%.1f%% tolerance=%.1f%%",
		actual, target, pct*100, tolerance*100)
	if pct > tolerance {
		t.Errorf("DB size %.1f%% off target (%d vs %d), tolerance %.1f%%",
			pct*100, actual, target, tolerance*100)
	}
}
