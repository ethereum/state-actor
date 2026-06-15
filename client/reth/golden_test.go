//go:build cgo_reth

package reth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/e2e_testing"
)

func TestRethGoldenStateRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reth-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := e2e_testing.GoldenStateRootCfg(dbPath)
	e2e_testing.AssertGoldenStateRoot(t, "reth", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return RunCgo(ctx, cfg, Options{})
		})
}
