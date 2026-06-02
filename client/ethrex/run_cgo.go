//go:build cgo_ethrex

package ethrex

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/genesisheader"
)

// runImpl is the cgo_ethrex orchestrator.
//
// Write order is the load-bearing invariant:
//  1. DB open (fresh-dir guard prevents orphan rows from partial runs).
//  2. writeState → trie + code CFs written, stateRoot obtained.
//  3. writeGenesisBlock: headers / bodies / block_numbers / chain_data written,
//     canonical_block_hashes written LAST with sync (boot gate).
//  4. WriteStoreMetadata: metadata.json written — ethrex refuses to boot a
//     non-empty dir without it.
//  5. WriteGenesisSidecar: ethrex-genesis.json for --network boot flag.
//  6. Close (CompactRange + flush).
func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("ethrex: --db is required")
	}

	if cfg.TrieMode == generator.TrieModeBinary {
		return nil, errors.New("ethrex: binary trie (EIP-7864) is not supported by ethrex")
	}
	if cfg.Archive {
		return nil, errors.New("ethrex: --archive is not supported by the ethrex writer")
	}

	g := cfg.Genesis
	if g == nil {
		g, _ = genesis.BuildSynthetic("osaka", nil, 0, 0, nil)
	}
	if g.Config == nil {
		return nil, errors.New("ethrex: cfg.Genesis must have Config set (use genesis.BuildSynthetic)")
	}

	db, err := openEthrexDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	accountSink := newBatchSink(db, cfIdxAccountTrieNodes)
	defer accountSink.Close()
	storageSink := newBatchSink(db, cfIdxStorageTrieNodes)
	defer storageSink.Close()

	stateRoot, stats, err := writeState(ctx, cfg, db, accountSink, storageSink)
	if err != nil {
		return nil, fmt.Errorf("ethrex: writeState: %w", err)
	}

	header := genesisheader.Build(g, 0, common.Hash{}, stateRoot)

	chainConfigJSON, err := ChainConfigJSON(g)
	if err != nil {
		return nil, fmt.Errorf("ethrex: ChainConfigJSON: %w", err)
	}

	if err := writeGenesisBlock(db, header, chainConfigJSON); err != nil {
		return nil, err
	}

	if err := WriteStoreMetadata(cfg.DBPath); err != nil {
		return nil, err
	}

	if err := WriteGenesisSidecar(cfg.DBPath, g); err != nil {
		return nil, err
	}

	stats.StateRoot = stateRoot
	return stats, nil
}
