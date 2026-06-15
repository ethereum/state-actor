package e2e_testing

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/autofill"
	"github.com/ethereum/state-actor/internal/entitygen"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// GoldenTestBudget is the auto-fill target budget every cross-client
// golden-root test runs against. Small enough to keep CI fast, large
// enough to produce a non-trivial Plan (NumContracts > 0).
const GoldenTestBudget = 256 << 10 // 256 KiB

// GoldenStateRootCfg returns the canonical Osaka-bootable config every
// MPT-mode client adapter must produce the same state root for: the
// auto-fill Plan that PlanForBudget(GoldenTestBudget) computes, with
// seed=12345. Callers supply DBPath and may set TrieMode / WriteTrieNodes /
// Verbose before passing the cfg to AssertGoldenStateRoot.
func GoldenStateRootCfg(dbPath string) generator.Config {
	plan, err := autofill.PlanForBudget(GoldenTestBudget)
	if err != nil {
		panic("e2e_testing: PlanForBudget for GoldenTestBudget should never error: " + err.Error())
	}
	return generator.Config{
		DBPath:   dbPath,
		AutoFill: plan,
		Seed:     12345,
	}
}

// AssertGoldenStateRoot adds the 4 canonical EIP system contracts to cfg,
// invokes runFn, and asserts the produced state root equals
// entitygen.CanonicalOsakaMPTRoot. Drift requires a coordinated update of
// CanonicalOsakaMPTRoot + every caller + internal/entitygen/canonical_mpt_test.go.
func AssertGoldenStateRoot(t *testing.T, clientName string, cfg generator.Config,
	runFn func(context.Context, generator.Config) (*generator.Stats, error)) {
	t.Helper()
	syscontracts.AddCanonicalSystemContracts(&cfg)

	stats, err := runFn(context.Background(), cfg)
	if err != nil {
		t.Fatalf("%s run: %v", clientName, err)
	}
	if stats == nil {
		t.Fatalf("%s run returned nil stats", clientName)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatalf("%s run returned zero state root", clientName)
	}
	want := entitygen.CanonicalOsakaMPTRoot.Hex()
	if got := stats.StateRoot.Hex(); got != want {
		t.Fatalf("%s golden state root mismatch:\n  got:  %s\n  want: %s\n  Drift requires coordinated update of entitygen.CanonicalOsakaMPTRoot + all 4 client goldens + internal/entitygen/canonical_mpt_test.go.",
			clientName, got, want)
	}
}
