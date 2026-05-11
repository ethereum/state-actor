package nethermind

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/genesis"
)

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

// TestWriteChainSpec_OsakaGating drives both gating branches: the keys in
// osakaParamKeys must be present when --fork=osaka and absent when --fork=prague.
func TestWriteChainSpec_OsakaGating(t *testing.T) {
	cases := []struct {
		fork    string
		present bool
	}{
		{"osaka", true},
		{"prague", false},
	}
	for _, tc := range cases {
		t.Run(tc.fork, func(t *testing.T) {
			params := paramsBlock(t, readWrittenSpec(t, tc.fork))
			for _, k := range osakaParamKeys {
				_, ok := params[k]
				if ok != tc.present {
					if tc.present {
						t.Errorf("--fork=%s spec missing osaka key %q", tc.fork, k)
					} else {
						t.Errorf("--fork=%s spec unexpectedly contains osaka key %q", tc.fork, k)
					}
				}
			}
		})
	}
}

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

// TestWriteChainSpec_Eip1153PresentInCancun guards against silent template
// drift: EIP-1153 (TSTORE/TLOAD) is Cancun-active but a missing key here
// would only surface when a contract actually uses transient storage.
func TestWriteChainSpec_Eip1153PresentInCancun(t *testing.T) {
	params := paramsBlock(t, readWrittenSpec(t, "osaka"))
	if _, ok := params["eip1153TransitionTimestamp"]; !ok {
		t.Error("eip1153TransitionTimestamp absent — template drift")
	}
}

// TestWriteChainSpec_EngineIsEthash guards the MergePlugin SealEngineType
// allowlist {BeaconChain, Clique, Ethash} — using NethDev (the legacy
// dev engine, not on the allowlist) would silently fail to seal blocks
// on a post-Prague chain with system contracts.
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
		t.Errorf("engine.NethDev still present")
	}
	params := paramsBlock(t, spec)
	if ttd := params["terminalTotalDifficulty"]; ttd != "0x0" {
		t.Errorf("terminalTotalDifficulty = %v; want 0x0 (merge-from-genesis)", ttd)
	}
}

// TestWriteChainSpec_ParityChainIDFormat pins lowercase Go-default hex
// formatting — Nethermind's parser would silently misread a strconv.FormatInt(16)
// "beef" (no 0x prefix) as decimal.
func TestWriteChainSpec_ParityChainIDFormat(t *testing.T) {
	params := paramsBlock(t, readWrittenSpec(t, "osaka"))
	if got := params["chainID"]; got != "0x539" {
		t.Errorf("chainID = %v; want 0x539 (decimal 1337)", got)
	}
}

func TestWriteChainSpec_NilGenesisRejected(t *testing.T) {
	if _, err := writeChainSpec(t.TempDir(), nil); err == nil {
		t.Error("writeChainSpec(nil) returned no error")
	}
}

// TestWriteChainSpec_FilePathIsConventional locks the output filename —
// smoke scripts hardcode parity-chainspec.json.
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

// TestWriteChainSpec_ByteForByteDeterministic catches encoder-order drift.
// The Osaka-gating loop iterates a slice (already deterministic), and
// json.MarshalIndent sorts map keys today — but a future encoder swap
// could silently break the cross-client genesis-root invariant.
func TestWriteChainSpec_ByteForByteDeterministic(t *testing.T) {
	read := func() []byte {
		g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
		if err != nil {
			t.Fatalf("BuildSynthetic: %v", err)
		}
		out, err := writeChainSpec(t.TempDir(), g)
		if err != nil {
			t.Fatalf("writeChainSpec: %v", err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return raw
	}
	if a, b := read(), read(); !bytes.Equal(a, b) {
		t.Errorf("writeChainSpec is non-deterministic\nfirst:\n%s\nsecond:\n%s", a, b)
	}
}
