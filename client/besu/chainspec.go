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
// from the in-memory cfg.Genesis.
//
// state-actor only emits post-Prague chains (genesis.BuildChainConfigForFork
// rejects pre-Prague), so the chainspec is unconditionally post-Merge:
// terminalTotalDifficulty=0 + shanghaiTime/cancunTime/pragueTime active at
// genesis (osakaTime when --fork=osaka). Block production at boot goes
// through the engine API — besu has no native post-Merge dev consensus
// (clique is broken post-Shanghai per hyperledger/besu#8532 and removed
// in besu 26.4.0); a mock-CL helper drives engine_forkchoiceUpdated +
// engine_newPayload from the test side.
//
// Both writer and chainspec MUST stay byte-identical on every header
// field besu rebuilds — besu's BesuControllerBuilder.getGenesisState →
// GenesisState.fromStorage path recomputes the genesis hash from chainspec
// + cached stateRoot and compares against VARIABLES["chainHeadHash"];
// any divergence trips "Supplied genesis block does not match chain
// data stored". `--genesis-state-hash-cache-enabled` at boot tells besu
// to trust the stored stateRoot rather than recompute from alloc (we
// always emit alloc={}).
func writeChainSpec(dbPath string, g *genesis.Genesis) (string, error) {
	if g == nil {
		return "", fmt.Errorf("besu writeChainSpec: nil genesis")
	}
	if g.Config == nil || g.Config.ShanghaiTime == nil ||
		g.Config.CancunTime == nil || g.Config.PragueTime == nil {
		return "", fmt.Errorf("besu writeChainSpec: genesis must be post-Prague (use genesis.BuildSynthetic)")
	}

	chainID := int64(1337)
	if g.Config.ChainID != nil {
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
	diffHex := "0x0"
	if g.Difficulty != nil {
		diffHex = fmt.Sprintf("0x%x", g.Difficulty.ToInt())
	}

	cfg := map[string]any{
		"chainId":           chainID,
		"londonBlock":       0,
		"contractSizeLimit": 2147483647,
		// Empty `ethash: {}` satisfies besu's "consensus mechanism
		// defined" check at parse without configuring PoW (the chain
		// transitions to engine API immediately because TTD=0). Same
		// shape besu's bundled future.json / experimental.json use:
		// ethash{} + terminalTotalDifficulty=0.
		"ethash": map[string]any{},
		// JSON integer (NOT hex string) — besu's getOptionalBigInteger
		// parses as base-10. "0x0" → NumberFormatException at boot.
		"terminalTotalDifficulty": 0,
		"shanghaiTime":            *g.Config.ShanghaiTime,
		"cancunTime":              *g.Config.CancunTime,
		"pragueTime":              *g.Config.PragueTime,
		// Prague (EIP-6110/7002/7251) requires these system contract
		// addresses — besu refuses to boot a Prague-active chain
		// without them ("Withdrawal Request Contract Address not found").
		// Mainnet values per the EIPs; same as besu's ephemery + lukso
		// configs ship.
		"depositContractAddress":              "0x00000000219ab540356cbb839cbe05303d7705fa",
		"withdrawalRequestContractAddress":    "0x00000961ef480eb55e80d19ad83579a64c007002",
		"consolidationRequestContractAddress": "0x0000bbddc7ce488642fb579f8b00f3a590007251",
	}
	if g.Config.OsakaTime != nil {
		cfg["osakaTime"] = *g.Config.OsakaTime
	}

	// Cancun introduces blobs; besu requires a blobSchedule for any
	// chain with cancunTime active. Mainnet activation params per
	// EIP-7691 BPO: target=3/max=6 for Cancun, 6/9 for Prague,
	// 9/12 for Osaka. State-actor's e2e suite doesn't actually use
	// blob txs (spamoor's erc20_bloater is regular calldata), but
	// besu refuses to boot a Cancun-active chainspec without a
	// blobSchedule.
	blobSchedule := map[string]any{
		"cancun": map[string]any{
			"target":                3,
			"max":                   6,
			"baseFeeUpdateFraction": 3338477,
		},
		"prague": map[string]any{
			"target":                6,
			"max":                   9,
			"baseFeeUpdateFraction": 5007716,
		},
	}
	if g.Config.OsakaTime != nil {
		blobSchedule["osaka"] = map[string]any{
			"target":                9,
			"max":                   12,
			"baseFeeUpdateFraction": 5007716,
		}
	}
	cfg["blobSchedule"] = blobSchedule

	spec := map[string]any{
		"config":     cfg,
		"nonce":      fmt.Sprintf("0x%x", uint64(g.Nonce)),
		"timestamp":  fmt.Sprintf("0x%x", uint64(g.Timestamp)),
		"extraData":  extraDataHex,
		"gasLimit":   fmt.Sprintf("0x%x", gasLimit),
		"difficulty": diffHex,
		"mixHash":    g.Mixhash.Hex(),
		"coinbase":   g.Coinbase.Hex(),
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
