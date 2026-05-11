package nethermind

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/genesis"
)

// readWrittenSpec runs writeChainSpec into a temp dir and returns the
// re-parsed JSON tree. Centralizes setup + parse so each test below
// stays focused on its specific assertion.
func readWrittenSpec(t *testing.T, fork string) map[string]any {
	t.Helper()
	g, err := genesis.BuildSynthetic(fork, big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(%q): %v", fork, err)
	}
	dir := t.TempDir()
	outPath, err := writeChainSpec(dir, g)
	if err != nil {
		t.Fatalf("writeChainSpec: %v", err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read written spec: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse written spec: %v", err)
	}
	return spec
}

func paramsBlock(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	params, ok := spec["params"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing params block")
	}
	return params
}

// TestWriteChainSpec_OsakaIncludesAllForkKeys locks in #55: with --fork=osaka
// every gated EIP timestamp (cancun + prague + osaka) and every gated
// system-contract address is emitted. Catches accidental removal of a
// known-active EIP, and would have caught the pre-existing eip1153 drift
// if it had been on the gated list.
func TestWriteChainSpec_OsakaIncludesAllForkKeys(t *testing.T) {
	params := paramsBlock(t, readWrittenSpec(t, "osaka"))
	for fork, keys := range nethEIPsByFork {
		for _, k := range keys {
			if _, ok := params[k]; !ok {
				t.Errorf("osaka spec missing %s key %q", fork, k)
			}
		}
	}
	for fork, keys := range nethSystemContractAddressesByFork {
		for _, k := range keys {
			if _, ok := params[k]; !ok {
				t.Errorf("osaka spec missing %s contract address %q", fork, k)
			}
		}
	}
}

// TestWriteChainSpec_PragueStripsOsakaKeys verifies that --fork=prague
// produces a Prague-only spec — cancun + prague keys present, osaka keys
// stripped. This is the regression #55 was filed to prevent.
func TestWriteChainSpec_PragueStripsOsakaKeys(t *testing.T) {
	params := paramsBlock(t, readWrittenSpec(t, "prague"))
	for _, k := range nethEIPsByFork["cancun"] {
		if _, ok := params[k]; !ok {
			t.Errorf("prague spec unexpectedly missing cancun key %q", k)
		}
	}
	for _, k := range nethEIPsByFork["prague"] {
		if _, ok := params[k]; !ok {
			t.Errorf("prague spec unexpectedly missing prague key %q", k)
		}
	}
	for _, k := range nethEIPsByFork["osaka"] {
		if _, ok := params[k]; ok {
			t.Errorf("prague spec unexpectedly contains osaka key %q", k)
		}
	}
	for _, k := range nethSystemContractAddressesByFork["prague"] {
		if _, ok := params[k]; !ok {
			t.Errorf("prague spec missing prague contract address %q", k)
		}
	}
}

// TestWriteChainSpec_OverrideChainID asserts the chainID + networkID
// fields flow from g.Config.ChainID rather than the template's literal
// value. Regression test for the existing behavior.
func TestWriteChainSpec_OverrideChainID(t *testing.T) {
	g, err := genesis.BuildSynthetic("osaka", big.NewInt(0xbeef), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	outPath, err := writeChainSpec(t.TempDir(), g)
	if err != nil {
		t.Fatalf("writeChainSpec: %v", err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	params := paramsBlock(t, spec)
	if got := params["chainID"]; got != "0xbeef" {
		t.Errorf("chainID = %v; want 0xbeef", got)
	}
	if got := params["networkID"]; got != "0xbeef" {
		t.Errorf("networkID = %v; want 0xbeef", got)
	}
}

// TestWriteChainSpec_Eip1153PresentInCancun explicitly checks the key
// that was missing from the template before this fix. EIP-1153 is
// Cancun's transient storage (TSTORE/TLOAD); its absence is a silent
// chainspec drift that doesn't surface until a contract uses TSTORE.
func TestWriteChainSpec_Eip1153PresentInCancun(t *testing.T) {
	params := paramsBlock(t, readWrittenSpec(t, "osaka"))
	if _, ok := params["eip1153TransitionTimestamp"]; !ok {
		t.Error("eip1153TransitionTimestamp absent — template drift bug regressed")
	}
}

// TestWriteChainSpec_EngineIsEthash locks in #56's chainspec swap: the
// engine block is Ethash (with mainnet difficulty params for parser
// acceptance) and NethDev is no longer present. NethDev was incompatible
// with MergePlugin's seal-engine allowlist {BeaconChain, Clique, Ethash}.
func TestWriteChainSpec_EngineIsEthash(t *testing.T) {
	spec := readWrittenSpec(t, "osaka")
	engine, ok := spec["engine"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing engine block")
	}
	if _, ok := engine["Ethash"]; !ok {
		t.Errorf("engine.Ethash missing; engine block = %v", engine)
	}
	if _, ok := engine["NethDev"]; ok {
		t.Errorf("engine.NethDev still present — chainspec swap regressed")
	}
	params := paramsBlock(t, spec)
	if ttd := params["terminalTotalDifficulty"]; ttd != "0x0" {
		t.Errorf("terminalTotalDifficulty = %v; want 0x0 (merge-from-genesis)", ttd)
	}
}

// TestWriteChainSpec_ParityChainIDFormat asserts the chainID hex string
// uses lowercase Go-default formatting. Locking in the format avoids
// surprises if a future refactor switches to strconv.FormatInt(16) etc.
func TestWriteChainSpec_ParityChainIDFormat(t *testing.T) {
	params := paramsBlock(t, readWrittenSpec(t, "osaka"))
	if got := params["chainID"]; got != "0x539" {
		t.Errorf("chainID = %v; want 0x539 (decimal 1337)", got)
	}
}

// TestWriteChainSpec_NilGenesisRejected verifies the writer errors on
// nil input rather than producing a garbage file.
func TestWriteChainSpec_NilGenesisRejected(t *testing.T) {
	if _, err := writeChainSpec(t.TempDir(), nil); err == nil {
		t.Error("writeChainSpec(nil) returned no error")
	}
}

// TestWriteChainSpec_FilePathIsConventional verifies the writer puts the
// output at <dbPath>/parity-chainspec.json — smoke scripts depend on this
// hardcoded filename.
func TestWriteChainSpec_FilePathIsConventional(t *testing.T) {
	g, _ := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	dir := t.TempDir()
	out, err := writeChainSpec(dir, g)
	if err != nil {
		t.Fatalf("writeChainSpec: %v", err)
	}
	want := filepath.Join(dir, ChainSpecFileName)
	if out != want {
		t.Errorf("writeChainSpec returned %q; want %q", out, want)
	}
}
