//go:build cgo_besu

package besu

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/autofill"
	"github.com/ethereum/state-actor/internal/besu/keys"
	"github.com/ethereum/state-actor/internal/e2e_testing"
)

func TestBesuGoldenStateRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "besu-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := e2e_testing.GoldenStateRootCfg(dbPath)
	e2e_testing.AssertGoldenStateRoot(t, "besu", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return Run(ctx, cfg, Options{})
		})
}

// TestBesuReproducibility verifies that the same config + seed produces the
// same on-disk worldRoot sentinel across runs. Guards against any latent
// non-determinism in entity generation, Pebble batch ordering, or trie
// commit traversal.
func TestBesuReproducibility(t *testing.T) {
	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{
		AutoFill: plan,
		Seed:     42,
	}

	runOnce := func() (common.Hash, []byte) {
		dbPath := filepath.Join(t.TempDir(), "besu-repro")
		if err := os.MkdirAll(dbPath, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		c := cfg
		c.DBPath = dbPath
		stats, err := Run(context.Background(), c, Options{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		// Read the persisted worldRoot sentinel as cross-check that the
		// stats hash actually landed on disk (catches any future bug where
		// stats reports a value that doesn't match what Besu would read).
		_, rootBytes, err := readWorldRoot(dbPath)
		if err != nil {
			t.Fatalf("readWorldRoot: %v", err)
		}
		return stats.StateRoot, rootBytes
	}

	statsRoot1, diskRoot1 := runOnce()
	statsRoot2, diskRoot2 := runOnce()

	if statsRoot1 != statsRoot2 {
		t.Fatalf("stats StateRoot non-deterministic: %s vs %s", statsRoot1.Hex(), statsRoot2.Hex())
	}
	if !bytes.Equal(diskRoot1, diskRoot2) {
		t.Fatalf("on-disk worldRoot non-deterministic: %x vs %x", diskRoot1, diskRoot2)
	}
	if !bytes.Equal(statsRoot1.Bytes(), diskRoot1) {
		t.Fatalf("stats vs on-disk root mismatch: stats=%s, disk=%x", statsRoot1.Hex(), diskRoot1)
	}
}

// readWorldRoot opens the produced RocksDB and reads the
// TRIE_BRANCH_STORAGE[WorldRootKey] sentinel. Returns both the hash form
// (for cross-check vs stats.StateRoot) and the raw bytes (for byte-equal
// comparison across reproducibility runs).
func readWorldRoot(datadir string) (common.Hash, []byte, error) {
	dbPath := filepath.Join(datadir, "database")

	// Match the writer's CF set so RocksDB can open the DB.
	cfNames := []string{
		string(keys.CFDefault),
		string(keys.CFBlockchain),
		string(keys.CFAccountInfoState),
		string(keys.CFCodeStorage),
		string(keys.CFAccountStorageStorage),
		string(keys.CFTrieBranchStorage),
		string(keys.CFTrieLogStorage),
		string(keys.CFVariables),
	}
	cfOpts := make([]*grocksdb.Options, len(cfNames))
	for i := range cfOpts {
		cfOpts[i] = grocksdb.NewDefaultOptions()
	}
	defer func() {
		for _, o := range cfOpts {
			o.Destroy()
		}
	}()

	dbOpts := grocksdb.NewDefaultOptions()
	defer dbOpts.Destroy()

	db, cfs, err := grocksdb.OpenDbColumnFamilies(dbOpts, dbPath, cfNames, cfOpts)
	if err != nil {
		return common.Hash{}, nil, err
	}
	defer func() {
		for _, h := range cfs {
			h.Destroy()
		}
		db.Close()
	}()

	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	got, err := db.GetCF(ro, cfs[cfIdxTrieBranchStorage], keys.WorldRootKey)
	if err != nil {
		return common.Hash{}, nil, err
	}
	defer got.Free()

	raw := make([]byte, got.Size())
	copy(raw, got.Data())
	return common.BytesToHash(raw), raw, nil
}
