//go:build cgo_neth

package nethermind

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/e2e_testing"
)

func TestNethGoldenStateRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "neth-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := e2e_testing.GoldenStateRootCfg(dbPath)
	cfg.TrieMode = generator.TrieModeMPT
	// The writer always emits the flat layout; the state root is computed by
	// the same StackTrie over the same full-RLP leaves, so it must equal the
	// cross-client canonical root. A divergence would mean the flat tee
	// corrupted the trie feeding.
	e2e_testing.AssertGoldenStateRoot(t, "nethermind", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return Run(ctx, cfg, Options{})
		})
}
