package reth

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/nerolation/state-actor/genesis"
)

// writeChainSpec writes a Reth-compatible chainspec JSON to outPath.
//
// The chainspec carries the chain config (chainID + hardfork timestamps)
// and the header bits reth needs to derive the genesis header (gasLimit,
// baseFeePerGas, difficulty, etc.). The `alloc` field is intentionally
// emitted as an empty object: state-actor direct-writes the genesis state
// into MDBX, and reth is launched with `--debug.skip-genesis-validation` so
// it trusts the DB-resident state instead of recomputing the genesis hash
// from alloc.
//
// g is the in-memory *genesis.Genesis built by genesis.BuildSynthetic in
// main.go. Its JSON tags already match reth's chainspec field names, so
// a json.Marshal round-trip with alloc forced to empty is sufficient.
func writeChainSpec(g *genesis.Genesis, outPath string) error {
	if g == nil {
		return fmt.Errorf("writeChainSpec: nil genesis")
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("marshal genesis: %w", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("unmarshal genesis: %w", err)
	}
	// Empty object (not nil/missing) so alloy_genesis's parser reads it as
	// "no genesis accounts in chainspec" rather than tripping on a missing
	// required field.
	spec["alloc"] = map[string]any{}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chainspec: %w", err)
	}
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write chainspec: %w", err)
	}
	return nil
}
