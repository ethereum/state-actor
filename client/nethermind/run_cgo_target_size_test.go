//go:build cgo_neth

package nethermind

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
)

// TestRunEmitsFullPlan asserts that Run emits exactly
// plan.NumEOAs + plan.NumContracts entities even when cfg.TargetSize is
// set well below the plan's projected footprint. Post-Fix-A, TargetSize
// is advisory — the autofill Plan is the single source of truth for
// entity counts, so all four MPT clients must produce identical entity
// streams given the same Plan.
//
// Pre-Fix-A this test would have failed: the dirSize early-stop in
// entitygen_cgo.go sampled the State + Code RocksDB sizes every 100
// contracts and stopped emission once the sum exceeded TargetSize.
// At small TargetSize values the gate fired during the first sample,
// truncating the plan to ~100 contracts — which is exactly the failure
// mode seen on the 100 GB bloatnet bench.
func TestRunEmitsFullPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full-plan test in -short mode")
	}
	dir := t.TempDir()

	// Genesis is required: writeChainSpec + the writer pipeline need a
	// non-nil g.Config. Osaka matches the e2e suite's default fork.
	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	plan, err := autofill.PlanForBudget(1 << 20) // 1 MiB → ~1.2K EOAs + ~20 contracts
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{
		DBPath:   filepath.Join(dir, "neth"),
		AutoFill: plan,
		Seed:     42,
		TrieMode: generator.TrieModeMPT,
		Genesis:  g,
		// TargetSize is set deliberately small (128 KiB) so the OLD
		// dirSize early-stop would have fired on the first sample. The
		// full plan MUST still emit.
		TargetSize: 128 << 10,
	}

	stats, err := Run(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
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
