package geth

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
)

// Options carries geth-specific tuning knobs. Reserved for future
// expansion (e.g. per-phase WAL toggles, dirSize sampling cadence);
// today the writer derives its tuning from defaults.
type Options struct{}

// GenesisFilePath / ChainIDOverride are package-level vars set from
// main.go before Populate is called, mirroring the besu/nethermind
// pattern. The MPT writer reads chain config from cfg.GenesisAccounts /
// cfg.GenesisStorage / cfg.GenesisCode (already pre-loaded by the
// CLI); these vars are kept for parity with the other clients but
// aren't yet consumed here.
var (
	GenesisFilePath string
	ChainIDOverride int64
)

// Populate is the public entry for `--client=geth` MPT mode. It
// orchestrates the two-phase direct-Pebble writer:
//
//  1. Open the production geth Pebble DB at <dbPath>.
//  2. Drive Phase 1 (entitygen → temp Pebble) + Phase 2 (sorted
//     iteration → production Pebble + MPT trie nodes), returning the
//     state root.
//  3. Persist PathDB metadata (StateID, PersistentStateID, SnapshotRoot,
//     completed-snapshot-generator marker) so geth boots cleanly.
//  4. If a genesis config was loaded by the CLI (cfg.GenesisAccounts or
//     a non-empty *genesis.Genesis loaded from --genesis), write the
//     genesis block and chain config so the DB is fully self-bootable.
//  5. Close the writer.
//
// Returns the same *generator.Stats shape as the legacy generator.New +
// gen.Generate path so main.go's summary printing works uniformly.
//
// MPT mode only. Binary-trie mode still routes through generator.New
// (binary_stack_trie.go is untouched per the spec).
func Populate(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = opts // reserved for future tuning knobs

	if cfg.TrieMode == generator.TrieModeBinary {
		return nil, fmt.Errorf("client/geth.Populate: binary-trie mode goes through generator.New, not this entry point")
	}

	w, err := NewWriter(cfg.DBPath, cfg.BatchSize, cfg.Workers)
	if err != nil {
		return nil, fmt.Errorf("client/geth.Populate: open writer: %w", err)
	}
	defer func() {
		// Best-effort close; primary errors are returned via the explicit
		// path below.
		_ = w.Close()
	}()

	stateRoot, stats, err := writeStateAndCollectRoot(ctx, cfg, w)
	if err != nil {
		return nil, err
	}

	// PathDB / snapshot metadata. Always written, even when --genesis is
	// absent, so a state-only DB can boot the dev geth fork without
	// `geth init`.
	if err := w.SetStateRoot(stateRoot, false); err != nil {
		return nil, fmt.Errorf("client/geth.Populate: set state root: %w", err)
	}

	// Genesis block + chain config (only if the CLI loaded one).
	if cfg.GenesisAccounts != nil || GenesisFilePath != "" {
		if g, err := loadGenesisIfPresent(); err != nil {
			return nil, fmt.Errorf("client/geth.Populate: load genesis: %w", err)
		} else if g != nil {
			ancientDir := filepath.Join(cfg.DBPath, "ancient")
			if _, err := WriteGenesisBlock(w.DB(), g, stateRoot, false, ancientDir); err != nil {
				return nil, fmt.Errorf("client/geth.Populate: write genesis block: %w", err)
			}
		}
	}

	return stats, nil
}

// loadGenesisIfPresent reads GenesisFilePath if set, applying the
// ChainIDOverride if non-zero. Returns (nil, nil) when no genesis file
// is configured — the caller should treat that as a state-only run.
func loadGenesisIfPresent() (*genesis.Genesis, error) {
	if GenesisFilePath == "" {
		return nil, nil
	}
	g, err := genesis.LoadGenesis(GenesisFilePath)
	if err != nil {
		return nil, err
	}
	if ChainIDOverride != 0 && g.Config != nil {
		g.Config.ChainID = big.NewInt(ChainIDOverride)
	}
	return g, nil
}
