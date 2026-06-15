package geth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/e2e_testing"
)

func TestGethGoldenStateRoot(t *testing.T) {
	cfg := e2e_testing.GoldenStateRootCfg(filepath.Join(t.TempDir(), "geth", "chaindata"))
	cfg.TrieMode = generator.TrieModeMPT
	cfg.WriteTrieNodes = true
	e2e_testing.AssertGoldenStateRoot(t, "geth", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return Populate(ctx, cfg, Options{})
		})
}
