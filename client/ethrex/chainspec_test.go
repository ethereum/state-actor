package ethrex_test

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/state-actor/client/ethrex"
	"github.com/ethereum/state-actor/genesis"
)

// TestWriteStoreMetadata verifies that WriteStoreMetadata writes a valid JSON
// file with schema_version=2.
func TestWriteStoreMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := ethrex.WriteStoreMetadata(dir); err != nil {
		t.Fatalf("WriteStoreMetadata: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}

	var parsed struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	if parsed.SchemaVersion != 2 {
		t.Errorf("schema_version: got %d, want 2", parsed.SchemaVersion)
	}
}

// TestWriteGenesisSidecar verifies that WriteGenesisSidecar writes valid JSON
// that round-trips correctly (config, gasLimit, etc. present).
func TestWriteGenesisSidecar(t *testing.T) {
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 0x17d7840, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	dir := t.TempDir()
	if err := ethrex.WriteGenesisSidecar(dir, g); err != nil {
		t.Fatalf("WriteGenesisSidecar: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "ethrex-genesis.json"))
	if err != nil {
		t.Fatalf("read ethrex-genesis.json: %v", err)
	}

	// Must parse as valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal genesis sidecar: %v", err)
	}

	// Must contain required top-level fields.
	for _, field := range []string{"config", "gasLimit", "difficulty", "coinbase", "alloc"} {
		if _, ok := parsed[field]; !ok {
			t.Errorf("genesis sidecar missing field %q", field)
		}
	}

	// config must contain chainId.
	cfgAny, ok := parsed["config"].(map[string]any)
	if !ok {
		t.Fatalf("config is not an object")
	}
	if _, ok := cfgAny["chainId"]; !ok {
		t.Error("config missing chainId")
	}
}

// TestChainConfigJSON verifies that ChainConfigJSON returns valid JSON with
// expected fork fields.
func TestChainConfigJSON(t *testing.T) {
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	data, err := ethrex.ChainConfigJSON(g)
	if err != nil {
		t.Fatalf("ChainConfigJSON: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal ChainConfigJSON: %v", err)
	}

	// Must have fork-time fields for Prague.
	for _, field := range []string{"shanghaiTime", "cancunTime", "pragueTime"} {
		if _, ok := parsed[field]; !ok {
			t.Errorf("ChainConfigJSON missing %q", field)
		}
	}

	// depositContractAddress is mandatory in ethrex's ChainConfig deserializer
	// (no serde default); the sidecar must always carry it or `ethrex --network`
	// fails to decode the genesis. BuildSynthetic leaves it zero, so this must be
	// the mainnet fallback.
	got, _ := parsed["depositContractAddress"].(string)
	if !strings.EqualFold(got, "0x00000000219ab540356cbb839cbe05303d7705fa") {
		t.Errorf("depositContractAddress: got %v, want mainnet fallback", got)
	}
}

// TestStoreDir locks the on-disk store path to ethrex's effective datadir
// (<datadir>/chain-<chainid>). Boot correctness depends on this exactly
// matching ethrex's compute_effective_datadir → datadir_suffix; a mismatch
// would silently send ethrex to a different (empty) dir or trigger migration.
func TestStoreDir(t *testing.T) {
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 0, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	if got, want := ethrex.StoreDir("/data", g), filepath.Join("/data", "chain-1337"); got != want {
		t.Errorf("StoreDir(1337) = %q, want %q", got, want)
	}

	g2, err := genesis.BuildSynthetic("prague", big.NewInt(424242), 0, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	if got, want := ethrex.StoreDir("/x", g2), filepath.Join("/x", "chain-424242"); got != want {
		t.Errorf("StoreDir(424242) = %q, want %q", got, want)
	}

	// nil genesis falls back to the buildGenesisJSON default chain id (1337).
	if got, want := ethrex.StoreDir("/data", nil), filepath.Join("/data", "chain-1337"); got != want {
		t.Errorf("StoreDir(nil) = %q, want %q", got, want)
	}
}
