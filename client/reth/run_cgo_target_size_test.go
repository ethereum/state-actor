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
	"github.com/nerolation/state-actor/internal/autofill"
)

// TestRunCgoEmitsFullPlan asserts that RunCgo emits exactly
// plan.NumEOAs + plan.NumContracts entities even when cfg.TargetSize is
// set well below the plan's projected footprint. Post-Fix-A, TargetSize
// is advisory — the autofill Plan is the single source of truth for
// entity counts, so all four MPT clients must produce identical entity
// streams given the same Plan.
//
// Pre-Fix-A this test would have failed: the dirSize early-stop in
// run_cgo.go fired once cfg.DBPath's apparent size exceeded TargetSize,
// truncating the plan mid-loop. For reth specifically the gate fired
// almost immediately (MDBX preallocates mdbx.dat in 4 GiB growth steps),
// which is why the bloatnet bench saw reth emit only 12 % of the plan.
func TestRunCgoEmitsFullPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full-plan test in -short mode")
	}
	dir := t.TempDir()

	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	plan, err := autofill.PlanForBudget(1 << 20) // 1 MiB → ~1.2K EOAs + ~20 contracts
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{
		DBPath:   filepath.Join(dir, "reth-datadir"),
		AutoFill: plan,
		Seed:     42,
		TrieMode: generator.TrieModeMPT,
		Genesis:  g,
		// TargetSize is set well below the MDBX preallocation step so
		// the OLD dirSize early-stop would have fired on the very first
		// post-batch sample. The full plan MUST still emit.
		TargetSize: 128 << 10,
	}

	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
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

// TestContractStreamBatchSize pins the contract batch size at 1 K.
// Post-Fix-A this is no longer a target-size overshoot guard (that gate
// is gone) but a MDBX commit-cadence pin: per-contract writes are ~35 KiB
// mean, so 1 K keeps txn working-set RAM bounded. Raising it would
// silently grow per-batch RAM in the contract loop.
func TestContractStreamBatchSize(t *testing.T) {
	const want = 1_000
	if contractStreamBatchSize != want {
		t.Errorf("contractStreamBatchSize = %d, want %d (this constant bounds per-batch MDBX commit working-set RAM)",
			contractStreamBatchSize, want)
	}
}
