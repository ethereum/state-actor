//go:build cgo_neth

package nethermind

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

// TestNethGoldenStateRoot pins the state root the full cgo_neth pipeline
// produces for the canonical Osaka-bootable config: 10 EOAs + 5 contracts
// (seed=12345, PowerLaw, MaxSlots=100, CodeSize=256) + 4 EIP system
// contracts via oracle.AddPragueSystemContracts. The hash MUST equal
// entitygen.CanonicalOsakaMPTRoot — every MPT-mode client adapter
// (geth, nethermind, besu, reth) pins the same constant. Drift requires
// a coordinated update across all 4 + the pure-Go canonical_mpt_test.
func TestNethGoldenStateRoot(t *testing.T) {
	expectedRoot := entitygen.CanonicalOsakaMPTRoot.Hex()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "neth-golden")
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
		TrieMode:     generator.TrieModeMPT,
		Verbose:      false,
	}
	// Deploy EIP-4788/2935/7002/7251 system contracts — match the
	// Osaka-bootable canonical.
	oracle.AddPragueSystemContracts(&cfg)

	stats, err := Run(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats == nil {
		t.Fatal("Run returned nil stats")
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatal("Run returned zero state root — pipeline didn't populate stats.StateRoot")
	}
	if got := stats.StateRoot.Hex(); got != expectedRoot {
		t.Fatalf("nethermind golden state root mismatch:\n  got:  %s\n  want: %s\n  Diverging here means a coordinated update across all 4 client goldens + internal/entitygen/canonical_mpt_test.go is needed.",
			got, expectedRoot)
	}
}
