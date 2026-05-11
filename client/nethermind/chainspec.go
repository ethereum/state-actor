package nethermind

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nerolation/state-actor/genesis"
)

// ChainSpecFileName is the on-disk filename for the Parity-style chainspec
// state-actor writes next to the DB. Smoke scripts point Nethermind at it
// via the Init config's ChainSpecPath; closes the B7 loop so --chain-id
// is no longer warn-and-ignored at boot.
const ChainSpecFileName = "parity-chainspec.json"

//go:embed testdata/chainspecs/sa-dev.json
var paritySpecTemplate []byte

// nethEIPsByFork groups Parity EIP-transition keys by fork. The embedded
// template ships every key with timestamp 0x0; the writer strips entries
// for forks not active in g.Config. Shanghai EIPs (eip3651/eip3855/eip3860/
// eip4895) are unconditional — genesis.BuildChainConfigForFork rejects pre-
// Prague, so Shanghai is always active. Source: Nethermind release notes
// cross-checked against
// src/Nethermind/Nethermind.Specs/ChainSpecStyle/ChainSpecBasedSpecProvider.cs.
var nethEIPsByFork = map[string][]string{
	"cancun": {
		"eip4788TransitionTimestamp",
		"eip4844TransitionTimestamp",
		"eip1153TransitionTimestamp",
		"eip5656TransitionTimestamp",
		"eip6780TransitionTimestamp",
	},
	"prague": {
		"eip2537TransitionTimestamp",
		"eip2935TransitionTimestamp",
		"eip6110TransitionTimestamp",
		"eip7002TransitionTimestamp",
		"eip7251TransitionTimestamp",
		"eip7623TransitionTimestamp",
		"eip7702TransitionTimestamp",
	},
	"osaka": {
		"eip7594TransitionTimestamp",
		"eip7823TransitionTimestamp",
		"eip7825TransitionTimestamp",
		"eip7883TransitionTimestamp",
		"eip7918TransitionTimestamp",
		"eip7934TransitionTimestamp",
		"eip7939TransitionTimestamp",
		"eip7951TransitionTimestamp",
	},
}

// nethSystemContractAddressesByFork groups Parity system-contract-address
// keys by fork. Stripped from params when the corresponding fork is inactive
// in g.Config. depositContractAddress is grouped with Prague because EIP-6110
// (validator deposits via EL system contract) activates at Prague.
var nethSystemContractAddressesByFork = map[string][]string{
	"prague": {
		"depositContractAddress",
		"eip7002ContractAddress",
		"eip7251ContractAddress",
	},
}

// writeChainSpec emits the embedded sa-dev Parity chainspec to
// <dbPath>/<ChainSpecFileName>, parameterized by g.Config:
//
//   - chainID + networkID flow from g.Config.ChainID.
//   - EIP transition timestamps + system-contract addresses are gated by
//     fork-active timestamps (g.Config.{Cancun,Prague,Osaka}Time != nil).
//     Inactive-fork keys are removed from params so Nethermind doesn't see
//     them as active-at-genesis.
//
// Engine is Ethash + terminalTotalDifficulty=0 — the Kiln/Kintsugi merge-
// from-genesis recipe. MergePlugin wraps EthashPlugin and routes every
// block through the engine API; EthashPlugin's PoW path never runs because
// TTD=0 means post-merge from block 0. NethDev was removed because it's
// not in MergePlugin's SealEngineType allowlist (cf. state-actor#56).
//
// State-actor only supports prague+osaka today (pre-Prague is EOL in
// genesis.BuildChainConfigForFork), so the cancun/prague gates have no
// observable effect — they're structural future-proofing matching the
// pattern in client/besu/chainspec.go.
func writeChainSpec(dbPath string, g *genesis.Genesis) (string, error) {
	if g == nil {
		return "", fmt.Errorf("nethermind writeChainSpec: nil genesis")
	}
	if g.Config == nil {
		return "", fmt.Errorf("nethermind writeChainSpec: g.Config required (use genesis.BuildSynthetic)")
	}
	chainID := int64(1337)
	if g.Config.ChainID != nil {
		chainID = g.Config.ChainID.Int64()
	}

	var spec map[string]any
	if err := json.Unmarshal(paritySpecTemplate, &spec); err != nil {
		return "", fmt.Errorf("nethermind writeChainSpec parse template: %w", err)
	}
	params, ok := spec["params"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("nethermind writeChainSpec: template missing params block")
	}
	hex := fmt.Sprintf("0x%x", chainID)
	params["chainID"] = hex
	params["networkID"] = hex

	forkActive := map[string]bool{
		"cancun": g.Config.CancunTime != nil,
		"prague": g.Config.PragueTime != nil,
		"osaka":  g.Config.OsakaTime != nil,
	}
	for fork, active := range forkActive {
		if active {
			continue
		}
		for _, k := range nethEIPsByFork[fork] {
			delete(params, k)
		}
		for _, k := range nethSystemContractAddressesByFork[fork] {
			delete(params, k)
		}
	}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("nethermind writeChainSpec marshal: %w", err)
	}
	outPath := filepath.Join(dbPath, ChainSpecFileName)
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("nethermind writeChainSpec write: %w", err)
	}
	return outPath, nil
}
