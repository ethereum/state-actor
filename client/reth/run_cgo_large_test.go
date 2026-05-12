//go:build cgo_reth && large

package reth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/generator"
)

// TestRunCgoStreamingMultiBatch exercises the Phase 4 streaming pipeline
// across many batches (200K accounts at batch size 50K → 4 batches). It
// catches any regression where a single-batch run looks fine but cross-
// batch RNG ordering or sorter draining breaks.
//
// Gated by the `large` build tag because it takes ~10s and writes ~200 MB
// of artifacts. Run via:
//
//	docker run --rm state-actor-reth go test -tags 'cgo_reth large' \
//	    -run TestRunCgoStreamingMultiBatch -v ./client/reth/
func TestRunCgoStreamingMultiBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("large run skipped in -short mode")
	}

	tmp := t.TempDir()
	cfg := generator.Config{
		DBPath:      tmp,
		NumAccounts: 200_000,
		Seed:        4242,
	}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats == nil {
		t.Fatal("RunCgo returned nil stats")
	}
	if stats.AccountsCreated != cfg.NumAccounts {
		t.Errorf("AccountsCreated = %d, want %d", stats.AccountsCreated, cfg.NumAccounts)
	}
	for _, rel := range []string{
		"db/mdbx.dat",
		"db/database.version",
		"chainspec.json",
		"rocksdb/CURRENT",
		"static_files",
	} {
		if _, err := os.Stat(filepath.Join(tmp, rel)); err != nil {
			t.Errorf("expected %s: %v", rel, err)
		}
	}

	// Sorter cleanup: the temp Pebble dir must be gone after RunCgo
	// returns, so the datadir doesn't leak GBs of internal state.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Base(e.Name()) == "" {
			continue
		}
		// reth-sort-* prefix matches NewSorter's MkdirTemp pattern.
		name := e.Name()
		if len(name) > len("reth-sort-") && name[:len("reth-sort-")] == "reth-sort-" {
			t.Errorf("Sorter temp dir leaked into datadir: %s", name)
		}
	}
}
