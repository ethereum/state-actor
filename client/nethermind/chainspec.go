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

// osakaParamKeys lists the Parity EIP-transition and system-contract keys
// the writer strips from the embedded template when --fork is below Osaka.
// genesis.BuildChainConfigForFork rejects pre-Prague, so cancun + prague
// + shanghai keys are always active and stay in the template unconditionally.
// Source: Nethermind release notes cross-checked against
// src/Nethermind/Nethermind.Specs/ChainSpecStyle/ChainSpecBasedSpecProvider.cs.
var osakaParamKeys = []string{
	"eip7594TransitionTimestamp",
	"eip7823TransitionTimestamp",
	"eip7825TransitionTimestamp",
	"eip7883TransitionTimestamp",
	"eip7918TransitionTimestamp",
	"eip7934TransitionTimestamp",
	"eip7939TransitionTimestamp",
	"eip7951TransitionTimestamp",
}

// writeChainSpec emits the embedded sa-dev Parity chainspec to
// <dbPath>/<ChainSpecFileName>, with chainID + networkID overridden from
// g.Config.ChainID and Osaka-only keys stripped if g.Config.OsakaTime is
// unset.
//
// Engine is Ethash + terminalTotalDifficulty=0 — the Kiln/Kintsugi merge-
// from-genesis recipe. MergePlugin wraps EthashPlugin and routes every
// block through the engine API; EthashPlugin's PoW path never runs because
// TTD=0 means post-merge from block 0. MergePlugin's SealEngineType
// allowlist is {BeaconChain, Clique, Ethash}, which is why we don't use
// NethDev here.
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

	if g.Config.OsakaTime == nil {
		for _, k := range osakaParamKeys {
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
