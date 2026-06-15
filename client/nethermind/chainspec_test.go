package nethermind

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ethereum/state-actor/genesis"
)

func readWrittenSpec(t *testing.T, fork string) map[string]any {
	t.Helper()
	g, err := genesis.BuildSynthetic(fork, big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(%q): %v", fork, err)
	}
	dir := t.TempDir()
	outPath, err := writeChainSpec(dir, g, common.Hash{})
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
	outPath, err := writeChainSpec(t.TempDir(), g, common.Hash{})
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
	if _, err := writeChainSpec(t.TempDir(), nil, common.Hash{}); err == nil {
		t.Error("writeChainSpec(nil) returned no error")
	}
}

// TestWriteChainSpec_FilePathIsConventional locks the output filename —
// smoke scripts hardcode parity-chainspec.json.
func TestWriteChainSpec_FilePathIsConventional(t *testing.T) {
	g, _ := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	dir := t.TempDir()
	out, err := writeChainSpec(dir, g, common.Hash{})
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
		// Fixed stateRoot so the byte-for-byte comparison stays deterministic.
		out, err := writeChainSpec(t.TempDir(), g, common.HexToHash("0xabcd"))
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

func parseHexUint(t *testing.T, v any) uint64 {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected hex string, got %T (%v)", v, v)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("parse hex %q: %v", s, err)
	}
	return n
}

// TestWriteChainSpec_GenesisBlockFromG locks the unification contract: every
// genesis-block field the on-disk header carries is also emitted to the
// chainspec from the same g. Drives every customizable field with a non-
// default value so a regression that re-introduces a literal in the template
// (or short-circuits the override) shows up here.
func TestWriteChainSpec_GenesisBlockFromG(t *testing.T) {
	const (
		wantGasLimit  uint64 = 20_000_000
		wantTimestamp uint64 = 1_700_000_000
	)
	wantExtraData := []byte("hello state-actor")

	g, err := genesis.BuildSynthetic("osaka", big.NewInt(0xc0de), wantGasLimit, wantTimestamp, wantExtraData)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	// Set the legacy fields that have no CLI flag but are still part of
	// the chainspec/header contract — POTENTIAL-divergence coverage.
	g.Mixhash = common.HexToHash("0x1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff")
	g.Coinbase = common.HexToAddress("0xcafe000000000000000000000000000000000000")
	wantStateRoot := common.HexToHash("0xe86fef3b0000000000000000000000000000000000000000000000000032b900")

	outPath, err := writeChainSpec(t.TempDir(), g, wantStateRoot)
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
	gen, ok := spec["genesis"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing genesis block; got %T", spec["genesis"])
	}

	if got := parseHexUint(t, gen["gasLimit"]); got != wantGasLimit {
		t.Errorf("gasLimit = %d; want %d", got, wantGasLimit)
	}
	if got := parseHexUint(t, gen["timestamp"]); got != wantTimestamp {
		t.Errorf("timestamp = %d; want %d", got, wantTimestamp)
	}
	if got := gen["extraData"]; got != hexutil.Encode(wantExtraData) {
		t.Errorf("extraData = %v; want %s", got, hexutil.Encode(wantExtraData))
	}
	if got := gen["author"]; got != g.Coinbase.Hex() {
		t.Errorf("author = %v; want %s", got, g.Coinbase.Hex())
	}
	if got := gen["parentHash"]; got != g.ParentHash.Hex() {
		t.Errorf("parentHash = %v; want %s", got, g.ParentHash.Hex())
	}
	if got := gen["difficulty"]; got != "0x0" {
		t.Errorf("difficulty = %v; want 0x0 (BuildSynthetic fixes to 0)", got)
	}

	seal, ok := gen["seal"].(map[string]any)
	if !ok {
		t.Fatalf("genesis.seal missing or wrong type")
	}
	eth, ok := seal["ethereum"].(map[string]any)
	if !ok {
		t.Fatalf("genesis.seal.ethereum missing or wrong type")
	}
	if got := eth["mixHash"]; got != g.Mixhash.Hex() {
		t.Errorf("mixHash = %v; want %s", got, g.Mixhash.Hex())
	}
	// Nethermind's parser requires fixed 8-byte / 16-hex-char nonce width.
	if got, _ := eth["nonce"].(string); got != "0x0000000000000000" || len(got) != 18 {
		t.Errorf("nonce = %q; want exactly %q (18 chars / 0x + 16 hex)", got, "0x0000000000000000")
	}
	// stateRoot + stateUnavailable=true gate Nethermind's GenesisBuilder
	// recompute path — without both, boot overwrites the on-disk root with
	// the empty-trie hash (issue #81).
	if got := gen["stateRoot"]; got != wantStateRoot.Hex() {
		t.Errorf("stateRoot = %v; want %s", got, wantStateRoot.Hex())
	}
	if got := gen["stateUnavailable"]; got != true {
		t.Errorf("stateUnavailable = %v (%T); want bool true", got, got)
	}
}
