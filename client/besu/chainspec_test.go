package besu

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/genesis"
)

func readChainSpec(t *testing.T, fork string) map[string]any {
	t.Helper()
	g, err := genesis.BuildSynthetic(fork, big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(%s): %v", fork, err)
	}
	dir := t.TempDir()
	path, err := writeChainSpec(dir, g)
	if err != nil {
		t.Fatalf("writeChainSpec: %v", err)
	}
	if path != filepath.Join(dir, ChainSpecFileName) {
		t.Errorf("path = %q, want %q", path, filepath.Join(dir, ChainSpecFileName))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return spec
}

func TestChainSpec_PostMergeFieldsAlwaysEmitted(t *testing.T) {
	spec := readChainSpec(t, "prague")
	cfg, _ := spec["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("config block missing: %v", spec)
	}

	// Post-Merge marker — TTD=0 means besu boots in engine-API mode.
	if got, want := cfg["terminalTotalDifficulty"], "0x0"; got != want {
		t.Errorf("terminalTotalDifficulty = %v, want %q", got, want)
	}
	// Mandatory fork-activation timestamps (cascaded from prague).
	for _, k := range []string{"shanghaiTime", "cancunTime", "pragueTime"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("config[%q] missing", k)
		}
	}
	// blobSchedule is required by besu once cancunTime is set.
	bs, ok := cfg["blobSchedule"].(map[string]any)
	if !ok {
		t.Fatalf("blobSchedule missing or not an object: %v", cfg["blobSchedule"])
	}
	for _, k := range []string{"cancun", "prague"} {
		if _, ok := bs[k]; !ok {
			t.Errorf("blobSchedule[%q] missing", k)
		}
	}

	// No PoW path. ethash is removed; clique is broken post-Shanghai
	// per hyperledger/besu#8532. No consensus engine block at all —
	// besu defaults to engine-API for post-Merge chains.
	for _, banned := range []string{"ethash", "clique", "qbft", "ibft2"} {
		if _, ok := cfg[banned]; ok {
			t.Errorf("config[%q] should not be emitted (post-Merge engine-API only)", banned)
		}
	}
}

func TestChainSpec_OsakaTimeOnlyWhenSelected(t *testing.T) {
	pragueSpec := readChainSpec(t, "prague")
	if _, ok := pragueSpec["config"].(map[string]any)["osakaTime"]; ok {
		t.Error("osakaTime should NOT be emitted when --fork=prague")
	}
	osakaSpec := readChainSpec(t, "osaka")
	cfg := osakaSpec["config"].(map[string]any)
	if _, ok := cfg["osakaTime"]; !ok {
		t.Error("osakaTime should be emitted when --fork=osaka")
	}
	bs := cfg["blobSchedule"].(map[string]any)
	if _, ok := bs["osaka"]; !ok {
		t.Error("blobSchedule[osaka] should be emitted when --fork=osaka")
	}
}

func TestChainSpec_RejectsPrePrague(t *testing.T) {
	// genesis.BuildSynthetic rejects pre-Prague at parse, so the chainspec
	// writer never sees a malformed input. Defensively verify writeChainSpec
	// also rejects a hand-constructed pre-Prague Genesis.
	g := &genesis.Genesis{
		Config: nil, // simulate pre-Prague (no shanghai/cancun/prague times)
	}
	if _, err := writeChainSpec(t.TempDir(), g); err == nil {
		t.Error("writeChainSpec should reject genesis without post-Prague config")
	}
}
