package geth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/state-actor/genesis"
)

// GenesisJSONFileName is the informational geth-format genesis dropped at the
// datadir root next to the populated DB.
//
// It is NOT load-bearing. geth boots from the chain config persisted inside
// the Pebble DB (rawdb.WriteChainConfig in WriteGenesisBlock), not from this
// file — that is the asymmetry vs besu/nethermind/reth, which each require an
// external chainspec at boot and consume theirs directly. This sidecar exists
// purely for parity/inspection: reading the chain config + genesis params, or
// `geth init`-ing a separate node with the same chain parameters.
const GenesisJSONFileName = "geth-genesis.json"

// writeGenesisJSON renders an informational geth-format genesis.json to the
// datadir root derived from dbPath (<datadir>/geth/chaindata → <datadir>) and
// returns the path written.
//
// IMPORTANT: alloc is emitted empty ({}). state-actor direct-writes the
// synthetic state into the trie rather than deriving it from alloc, so
// `geth init` against this file produces a chain with the right config but an
// EMPTY state — a different state root than the populated DB. The file
// captures chain config + genesis block params (chainId, fork schedule,
// gasLimit, timestamp, baseFee, …), never the generated state.
func writeGenesisJSON(dbPath string, g *genesis.Genesis) (string, error) {
	if g == nil {
		return "", fmt.Errorf("geth writeGenesisJSON: nil genesis")
	}

	// Round-trip through a map so the caller's *Genesis is never mutated and
	// alloc can be forced to an empty object rather than null. Mirrors
	// client/reth.writeChainSpec's approach.
	raw, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("geth writeGenesisJSON marshal: %w", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", fmt.Errorf("geth writeGenesisJSON unmarshal: %w", err)
	}
	spec["alloc"] = map[string]any{}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("geth writeGenesisJSON marshal indent: %w", err)
	}

	outPath := filepath.Join(gethDatadir(dbPath), GenesisJSONFileName)
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("geth writeGenesisJSON write: %w", err)
	}
	return outPath, nil
}

// DatadirRoot returns the geth datadir root for a given --db chaindata path
// (<datadir>/geth/chaindata → <datadir>). Exported so callers that drop
// sidecars at the datadir root (e.g. the run manifest) resolve the same
// location as the geth-genesis.json sidecar.
func DatadirRoot(dbPath string) string {
	return gethDatadir(dbPath)
}

// gethDatadir derives the geth datadir root from the chaindata path. By
// convention state-actor's --db ends in <datadir>/geth/chaindata, so the
// datadir is two levels up. If dbPath does not follow that layout, the sidecar
// lands next to dbPath instead — still discoverable, never an error.
func gethDatadir(dbPath string) string {
	if filepath.Base(filepath.Dir(dbPath)) == "geth" {
		return filepath.Dir(filepath.Dir(dbPath))
	}
	return dbPath
}
