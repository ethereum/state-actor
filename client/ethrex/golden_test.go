//go:build cgo_ethrex

package ethrex_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/client/ethrex"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/e2e_testing"
)

// TestEthrexGoldenStateRoot pins ethrex to entitygen.CanonicalOsakaMPTRoot
// for the canonical auto-fill config. Any drift in the ethrex writer that
// shifts the state root will fail here and require a coordinated update of
// CanonicalOsakaMPTRoot + all client goldens.
//
// Runs only with -tags cgo_ethrex (requires librocksdb, Docker-only).
// The Docker build wires this into the cgo-suite CI job alongside
// TestE2ESuite so both the golden invariant and the full boot path run in CI.
func TestEthrexGoldenStateRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ethrex-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := e2e_testing.GoldenStateRootCfg(dbPath)
	e2e_testing.AssertGoldenStateRoot(t, "ethrex", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return ethrex.Run(ctx, cfg, ethrex.Options{})
		})
}
