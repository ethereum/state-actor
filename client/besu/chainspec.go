package besu

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nerolation/state-actor/genesis"
)

// ChainSpecFileName is the on-disk filename for the besu chainspec written
// by writeChainSpec. Lives directly under cfg.DBPath so smoke scripts can
// pass --genesis-file=<dbPath>/besu-chainspec.json without juggling extra
// mounts.
const ChainSpecFileName = "besu-chainspec.json"

// writeChainSpec renders a Besu-bootable chainspec JSON to <dbPath>/<file>
// from the in-memory cfg.Genesis. Embeds chainID end-to-end so --chain-id
// is no longer warn-and-ignored at boot (closes the B7 loop).
//
// Field shape mirrors the working testdata/genesis-funded.json template:
//   - config: chainId + londonBlock=0 + contractSizeLimit + ethash.fixeddifficulty
//     (the ethash.fixeddifficulty stanza enables dev-mode mining with
//      Besu's `--miner-enabled` flag without an external CL).
//   - header: gasLimit / timestamp / extraData / difficulty / coinbase /
//     mixHash / nonce / baseFeePerGas (when London active).
//   - alloc: always {} — state-actor writes the state directly to RocksDB
//     and the smoke scripts pass `--genesis-state-hash-cache-enabled` so
//     Besu trusts the DB-resident state instead of recomputing from alloc.
func writeChainSpec(dbPath string, g *genesis.Genesis) (string, error) {
	if g == nil {
		return "", fmt.Errorf("besu writeChainSpec: nil genesis")
	}
	chainID := int64(1337)
	if g.Config != nil && g.Config.ChainID != nil {
		chainID = g.Config.ChainID.Int64()
	}
	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}
	extraDataHex := "0x"
	if len(g.ExtraData) > 0 {
		extraDataHex = "0x" + bytesToHex(g.ExtraData)
	}
	baseFeeHex := ""
	if g.BaseFee != nil {
		baseFeeHex = (*g.BaseFee).String()
	}

	cfg := map[string]any{
		"chainId":           chainID,
		"londonBlock":       0,
		"contractSizeLimit": 2147483647,
		"ethash": map[string]any{
			"fixeddifficulty": 100,
		},
	}
	spec := map[string]any{
		"config":     cfg,
		"nonce":      fmt.Sprintf("0x%x", uint64(g.Nonce)),
		"timestamp":  fmt.Sprintf("0x%x", uint64(g.Timestamp)),
		"extraData":  extraDataHex,
		"gasLimit":   fmt.Sprintf("0x%x", gasLimit),
		"difficulty": "0x10000", // matches besu testdata; ethash.fixeddifficulty actually controls
		"mixHash":    "0x0000000000000000000000000000000000000000000000000000000000000000",
		"coinbase":   "0x0000000000000000000000000000000000000000",
		"alloc":      map[string]any{},
	}
	if baseFeeHex != "" {
		spec["baseFeePerGas"] = baseFeeHex
	}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("besu writeChainSpec marshal: %w", err)
	}
	outPath := filepath.Join(dbPath, ChainSpecFileName)
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("besu writeChainSpec write: %w", err)
	}
	return outPath, nil
}

func bytesToHex(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0F]
	}
	return string(out)
}
