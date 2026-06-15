package nethermind

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ethereum/state-actor/genesis"
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
// <dbPath>/<ChainSpecFileName>. Everything that varies per-run flows from g:
// chainID/networkID from g.Config.ChainID, the Osaka-only param keys gated
// by g.Config.OsakaTime, and the genesis block (gasLimit/timestamp/extraData/
// nonce/mixHash/difficulty/author/parentHash) materialized from g.* so the
// chainspec and the on-disk header agree byte-for-byte — otherwise Nethermind
// rejects the DB at boot with "Supplied genesis block does not match chain
// data stored".
//
// stateRoot is the writer-computed state root; it is stamped into the
// emitted "genesis" block alongside stateUnavailable: true so Nethermind's
// GenesisBuilder.Build() does NOT recompute the root from chainspec.accounts
// (which is {}) and overwrite the on-disk header with the empty-trie hash.
//
// Engine is Ethash + terminalTotalDifficulty=0 — the Kiln/Kintsugi merge-
// from-genesis recipe. MergePlugin wraps EthashPlugin and routes every
// block through the engine API; EthashPlugin's PoW path never runs because
// TTD=0 means post-merge from block 0. MergePlugin's SealEngineType
// allowlist is {BeaconChain, Clique, Ethash}, which is why we don't use
// NethDev here.
func writeChainSpec(dbPath string, g *genesis.Genesis, stateRoot common.Hash) (string, error) {
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

	spec["genesis"] = genesisBlockFromG(g, stateRoot)

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

// genesisBlockFromG renders the Parity-style "genesis" sub-object from g.
// Field shape matches Nethermind.Specs.ChainSpecStyle.Json.ChainSpecGenesisJson —
// seal.ethereum.{nonce,mixHash} nested, author for coinbase, fixed 8-byte
// nonce width. Every value is sourced from g so the chainspec and the
// on-disk header (built by internal/genesisheader.Build from the same g)
// cannot diverge.
//
// stateRoot + stateUnavailable=true are emitted so Nethermind's
// GenesisBuilder.Build() treats the writer's root as authoritative instead
// of recomputing from chainspec.accounts (= {}) and overwriting the header.
func genesisBlockFromG(g *genesis.Genesis, stateRoot common.Hash) map[string]any {
	difficulty := big.NewInt(0)
	if g.Difficulty != nil {
		difficulty = g.Difficulty.ToInt()
	}
	return map[string]any{
		"seal": map[string]any{
			"ethereum": map[string]any{
				// Nethermind's parser requires exactly 8 bytes / 16 hex chars.
				"nonce":   fmt.Sprintf("0x%016x", uint64(g.Nonce)),
				"mixHash": g.Mixhash.Hex(),
			},
		},
		"difficulty":       fmt.Sprintf("0x%x", difficulty),
		"author":           g.Coinbase.Hex(),
		"timestamp":        fmt.Sprintf("0x%x", uint64(g.Timestamp)),
		"parentHash":       g.ParentHash.Hex(),
		"extraData":        hexutil.Encode([]byte(g.ExtraData)),
		"gasLimit":         fmt.Sprintf("0x%x", uint64(g.GasLimit)),
		"stateRoot":        stateRoot.Hex(),
		"stateUnavailable": true,
	}
}
