package geth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/oracle"
)

// TestGethGoldenStateRoot pins the state root the full geth Populate
// pipeline produces for the canonical Osaka-bootable config: 10 EOAs +
// 5 contracts (seed=12345, PowerLaw, MaxSlots=100, CodeSize=256) + 4
// EIP system contracts via oracle.AddPragueSystemContracts. The hash
// MUST equal entitygen.CanonicalOsakaMPTRoot — every MPT-mode client
// adapter (geth, nethermind, besu, reth) pins the same constant. Drift
// requires a coordinated update across all 4 + the pure-Go
// canonical_mpt_test.
//
// This is the load-bearing cross-client invariant test for geth-MPT.
func TestGethGoldenStateRoot(t *testing.T) {
	expectedRoot := entitygen.CanonicalOsakaMPTRoot.Hex()

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
	// Deploy EIP-4788/2935/7002/7251 system contracts — match the
	// Osaka-bootable canonical.
	oracle.AddPragueSystemContracts(&cfg)

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if got := stats.StateRoot.Hex(); got != expectedRoot {
		t.Fatalf("geth golden state root mismatch:\n  got:  %s\n  want: %s\n  Diverging here means a coordinated update across all 4 client goldens + internal/entitygen/canonical_mpt_test.go is needed.",
			got, expectedRoot)
	}
}
