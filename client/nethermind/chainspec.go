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

// writeChainSpec emits the embedded sa-dev Parity chainspec to
// <dbPath>/<ChainSpecFileName> with chainID + networkID overridden from
// cfg.Genesis.Config.ChainID. The template's EIP-transition timestamps
// remain at 0 (Shanghai-active) — Nethermind currently has no post-Merge
// header writer here so going past Shanghai needs additional work.
func writeChainSpec(dbPath string, g *genesis.Genesis) (string, error) {
	if g == nil {
		return "", fmt.Errorf("nethermind writeChainSpec: nil genesis")
	}
	chainID := int64(1337)
	if g.Config != nil && g.Config.ChainID != nil {
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
