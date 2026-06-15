//go:build cgo_neth

package nethermind

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
)

// TestRunPopulatesByteStats is the per-writer companion to
// main_test.go:TestMainBenchmarkPrintsStats. It exercises the
// writeSyntheticAccounts path end-to-end through Run() and pins the byte
// accounting (Account + Storage + Code) so a regression that drops any of
// the three increments fails at unit-test level instead of silently in
// --benchmark output. See issue #70.
//
// Uses the smallest config that produces non-zero entries across all three
// categories: 5 EOAs + 2 contracts with code + at least one storage slot
// each.
func TestRunPopulatesByteStats(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cgo stats test in -short mode")
	}
	dir := t.TempDir()

	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{
		DBPath:   filepath.Join(dir, "neth"),
		AutoFill: plan,
		Seed:     42,
		TrieMode: generator.TrieModeMPT,
		Genesis:  g,
	}

	stats, err := Run(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.AccountBytes == 0 {
		t.Errorf("stats.AccountBytes == 0 (writeSyntheticAccounts dropped account accounting)")
	}
	if stats.StorageBytes == 0 {
		t.Errorf("stats.StorageBytes == 0 (writeSyntheticAccounts dropped storage accounting)")
	}
	if stats.CodeBytes == 0 {
		t.Errorf("stats.CodeBytes == 0 (writeSyntheticAccounts dropped code accounting)")
	}
	if stats.TotalBytes != stats.AccountBytes+stats.StorageBytes+stats.CodeBytes {
		t.Errorf("stats.TotalBytes = %d, want sum %d", stats.TotalBytes,
			stats.AccountBytes+stats.StorageBytes+stats.CodeBytes)
	}
}
