package geth

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/genesis"
)

// TestWriteGenesisJSON verifies the informational geth-genesis.json lands at
// the datadir root, carries an empty alloc, round-trips as a valid geth
// genesis, and does not mutate the caller's *Genesis.
func TestWriteGenesisJSON(t *testing.T) {
	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(osaka): %v", err)
	}

	datadir := t.TempDir()
	dbPath := filepath.Join(datadir, "geth", "chaindata")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir chaindata: %v", err)
	}

	outPath, err := writeGenesisJSON(dbPath, g)
	if err != nil {
		t.Fatalf("writeGenesisJSON: %v", err)
	}

	// Sidecar belongs at the datadir root (two levels up from chaindata),
	// not buried inside the chaindata dir.
	wantPath := filepath.Join(datadir, GenesisJSONFileName)
	if outPath != wantPath {
		t.Errorf("outPath: got %q, want %q", outPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected genesis file at %q: %v", wantPath, err)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read genesis file: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal genesis file: %v", err)
	}

	// alloc must be an empty object — state is direct-written, not derived
	// from alloc; a non-empty alloc here would misrepresent the DB.
	alloc, ok := spec["alloc"].(map[string]any)
	if !ok {
		t.Fatalf("alloc: got %T, want object", spec["alloc"])
	}
	if len(alloc) != 0 {
		t.Errorf("alloc: got %d entries, want 0 (empty)", len(alloc))
	}

	// config block present with the chain ID we built with.
	cfg, ok := spec["config"].(map[string]any)
	if !ok {
		t.Fatalf("config: got %T, want object", spec["config"])
	}
	if cid, _ := cfg["chainId"].(float64); int64(cid) != 1337 {
		t.Errorf("config.chainId: got %v, want 1337", cfg["chainId"])
	}

	// Round-trips back through the loader as a valid geth genesis.
	if _, err := genesis.LoadGenesis(wantPath); err != nil {
		t.Errorf("LoadGenesis(written file): %v", err)
	}
}

// TestGethDatadir covers the chaindata→datadir derivation, including the
// non-conventional fallback.
func TestGethDatadir(t *testing.T) {
	tests := []struct {
		name   string
		dbPath string
		want   string
	}{
		{
			name:   "conventional geth/chaindata layout",
			dbPath: filepath.Join("/tmp", "sa-geth", "geth", "chaindata"),
			want:   filepath.Join("/tmp", "sa-geth"),
		},
		{
			name:   "non-conventional path falls back to dbPath",
			dbPath: filepath.Join("/tmp", "sa-geth", "weird"),
			want:   filepath.Join("/tmp", "sa-geth", "weird"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gethDatadir(tt.dbPath); got != tt.want {
				t.Errorf("gethDatadir(%q): got %q, want %q", tt.dbPath, got, tt.want)
			}
		})
	}
}
