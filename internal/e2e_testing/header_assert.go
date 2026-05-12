package e2e_testing

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// AssertGenesisHeaderMatches is the end-to-end proof of the unification
// contract closed by #51 (PRs A/B/C): chainspec writer and on-disk header
// both read every customizable field from g, so the EL re-emits the same
// values over JSON-RPC. Any divergence — hardcoded literal in a chainspec
// writer, short-circuited override, parser quirk — surfaces here instead
// of as a confusing boot failure further down the test.
//
// Asserts the eight fields that have a corresponding *Genesis field:
//
//   - eth_chainId                                   == g.Config.ChainID
//   - eth_getBlockByNumber("0x0").gasLimit          == g.GasLimit
//   - eth_getBlockByNumber("0x0").timestamp         == g.Timestamp
//   - eth_getBlockByNumber("0x0").extraData (hex)   == g.ExtraData
//   - eth_getBlockByNumber("0x0").parentHash        == g.ParentHash    (zero in our pipeline)
//   - eth_getBlockByNumber("0x0").mixHash           == g.Mixhash       (zero in our pipeline)
//   - eth_getBlockByNumber("0x0").miner             == g.Coinbase      (zero in our pipeline)
//   - eth_getBlockByNumber("0x0").difficulty        == g.Difficulty    (0 in our pipeline)
//
// All mismatches are reported in one call via t.Errorf so callers see
// every field's status, not just the first failure.
func AssertGenesisHeaderMatches(t *testing.T, rpcURL string, g *genesis.Genesis) {
	t.Helper()

	if g == nil || g.Config == nil {
		t.Fatalf("AssertGenesisHeaderMatches: nil genesis or g.Config")
	}

	// eth_chainId — client metadata, not from the block header.
	wantChainID := g.Config.ChainID.Uint64()
	raw, err := rpcprobe.Call(rpcURL, "eth_chainId", []any{})
	if err != nil {
		t.Errorf("eth_chainId: %v", err)
	} else {
		var hexStr string
		if jerr := jsonUnmarshalString(raw, &hexStr); jerr != nil {
			t.Errorf("eth_chainId: parse %s: %v", raw, jerr)
		} else if got, perr := parseHexUint64(hexStr); perr != nil {
			t.Errorf("eth_chainId: parse %q: %v", hexStr, perr)
		} else if got != wantChainID {
			t.Errorf("eth_chainId = %d (0x%x); want %d (0x%x)", got, got, wantChainID, wantChainID)
		}
	}

	// Genesis block header — the eight fields with a *Genesis counterpart.
	block, err := rpcprobe.BlockByNumber(rpcURL, "0x0")
	if err != nil {
		t.Fatalf("eth_getBlockByNumber(0x0): %v", err)
	}

	if got, perr := parseHexUint64(block.GasLimit); perr != nil {
		t.Errorf("block.gasLimit %q: %v", block.GasLimit, perr)
	} else if got != uint64(g.GasLimit) {
		t.Errorf("block.gasLimit = %d (0x%x); want %d (0x%x)", got, got, uint64(g.GasLimit), uint64(g.GasLimit))
	}

	if got, perr := parseHexUint64(block.Timestamp); perr != nil {
		t.Errorf("block.timestamp %q: %v", block.Timestamp, perr)
	} else if got != uint64(g.Timestamp) {
		t.Errorf("block.timestamp = %d (0x%x); want %d (0x%x)", got, got, uint64(g.Timestamp), uint64(g.Timestamp))
	}

	wantExtraData := hexutil.Encode([]byte(g.ExtraData))
	if !strings.EqualFold(block.ExtraData, wantExtraData) {
		t.Errorf("block.extraData = %q; want %q", block.ExtraData, wantExtraData)
	}

	if block.ParentHash != g.ParentHash {
		t.Errorf("block.parentHash = %s; want %s", block.ParentHash.Hex(), g.ParentHash.Hex())
	}
	if block.MixHash != g.Mixhash {
		t.Errorf("block.mixHash = %s; want %s", block.MixHash.Hex(), g.Mixhash.Hex())
	}
	if block.Miner != g.Coinbase {
		t.Errorf("block.miner = %s; want %s", block.Miner.Hex(), g.Coinbase.Hex())
	}

	wantDifficulty := big.NewInt(0)
	if g.Difficulty != nil {
		wantDifficulty = g.Difficulty.ToInt()
	}
	gotDifficulty, perr := hexutil.DecodeBig(block.Difficulty)
	if perr != nil {
		t.Errorf("block.difficulty %q: %v", block.Difficulty, perr)
	} else if gotDifficulty.Cmp(wantDifficulty) != 0 {
		t.Errorf("block.difficulty = %s; want %s", gotDifficulty.String(), wantDifficulty.String())
	}
}

// parseHexUint64 decodes "0x"-prefixed hex into a uint64. Accepts mixed
// case; rejects empty / missing-prefix / overflow with a useful error.
func parseHexUint64(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty hex string")
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return 0, fmt.Errorf("missing 0x prefix: %q", s)
	}
	return strconv.ParseUint(s[2:], 16, 64)
}

// jsonUnmarshalString decodes a JSON string token into out without
// allocating an intermediate map. raw must be a quoted JSON string
// (e.g. `"0x539"`).
func jsonUnmarshalString(raw []byte, out *string) error {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return fmt.Errorf("not a JSON string: %s", raw)
	}
	*out = string(raw[1 : len(raw)-1])
	return nil
}
