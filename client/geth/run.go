package geth

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
)

// Options carries geth-specific tuning knobs. Reserved for future
// expansion (e.g. per-phase WAL toggles, dirSize sampling cadence);
// today the writer derives its tuning from defaults.
type Options struct{}

// Populate is the public entry for `--client=geth` MPT mode. It
// orchestrates the two-phase direct-Pebble writer:
//
//  1. Open the production geth Pebble DB at <dbPath>.
//  2. Drive Phase 1 (entitygen → temp Pebble) + Phase 2 (sorted
//     iteration → production Pebble + MPT trie nodes), returning the
//     state root.
//  3. Persist PathDB metadata (StateID, PersistentStateID, SnapshotRoot,
//     completed-snapshot-generator marker) so geth boots cleanly.
//  4. Write the genesis block + chain config from cfg.Genesis (always
//     non-nil after main.go's BuildSynthetic call) so the DB is
//     fully self-bootable.
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

	// Genesis block + chain config from cfg.Genesis (synthesized by
	// main.go via genesis.BuildSynthetic; tests can leave it nil and
	// get the default chainspec).
	g := genesis.OrDefault(cfg.Genesis)
	ancientDir := filepath.Join(cfg.DBPath, "ancient")
	if _, err := WriteGenesisBlock(w.DB(), g, stateRoot, false, ancientDir); err != nil {
		return nil, fmt.Errorf("client/geth.Populate: write genesis block: %w", err)
	}

	return stats, nil
}
