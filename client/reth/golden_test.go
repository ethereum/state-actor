//go:build cgo_reth

package reth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/oracle"
)

// TestRethGoldenStateRoot pins the state root the full cgo_reth pipeline
// produces for the canonical Osaka-bootable config: 10 EOAs + 5 contracts
// (seed=12345, PowerLaw, MaxSlots=100, CodeSize=256) + 4 EIP system
// contracts via oracle.AddPragueSystemContracts. The hash MUST equal
// entitygen.CanonicalOsakaMPTRoot — every MPT-mode client adapter
// (geth, nethermind, besu, reth) pins the same constant. Drift requires
// a coordinated update across all 4 + the pure-Go canonical_mpt_test.
//
// This test is what catches the slot-count RNG drift. Reth previously
// computed `slotCount = (MinSlots+MaxSlots)/2` once outside the RNG draw,
// producing a different state root than besu/nethermind/canonical for any
// non-empty contract alloc. Fixed by drawing slotCount per-contract via
// entitygen.GenerateSlotCount in client/reth/run_cgo.go.
func TestRethGoldenStateRoot(t *testing.T) {
	expectedRoot := entitygen.CanonicalOsakaMPTRoot.Hex()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "reth-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := generator.Config{
		DBPath:       dbPath,
		NumAccounts:  10,
		NumContracts: 5,
		MaxSlots:     100,
		MinSlots:     1,
		Distribution: generator.PowerLaw,
		Seed:         12345,
		CodeSize:     256,
		Verbose:      false,
	}
	// Deploy EIP-4788/2935/7002/7251 system contracts — match the
	// Osaka-bootable canonical.
	oracle.AddPragueSystemContracts(&cfg)

	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats == nil {
		t.Fatal("RunCgo returned nil stats")
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatal("RunCgo returned zero state root — pipeline didn't populate stats.StateRoot")
	}
	if got := stats.StateRoot.Hex(); got != expectedRoot {
		t.Fatalf("reth golden state root mismatch:\n  got:  %s\n  want: %s\n  Diverging here means a coordinated update across all entitygen-using adapters is needed (see internal/entitygen/canonical_mpt_test.go).",
			got, expectedRoot)
	}
}
