//go:build cgo_ethrex

package ethrex

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/genesisheader"
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
		// Fork hardcoded to ethrex's MaxForkForClient ("osaka"). Only reached
		// by tests that omit a genesis; main.go always supplies one.
		var err error
		g, err = genesis.BuildSynthetic("osaka", nil, 0, 0, nil)
		if err != nil {
			return nil, fmt.Errorf("ethrex: build default genesis: %w", err)
		}
	}
	if g.Config == nil {
		return nil, errors.New("ethrex: cfg.Genesis must have Config set (use genesis.BuildSynthetic)")
	}

	// Write the RocksDB + metadata.json into the subdir ethrex resolves for a
	// custom --network genesis, so a boot at --datadir=<DBPath> finds the store
	// directly (no reliance on ethrex's datadir migration). See StoreDir.
	dbDir := StoreDir(cfg.DBPath, g)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("ethrex: create datadir %s: %w", dbDir, err)
	}

	db, err := openEthrexDB(dbDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	accountSink := newBatchSink(db, cfIdxAccountTrieNodes)
	defer accountSink.Close()
	storageSink := newBatchSink(db, cfIdxStorageTrieNodes)
	defer storageSink.Close()

	// Flat-KV sinks: every leaf full-path row also lands in the flat-KV CFs so
	// the produced DB models a synced node (see writeState + ethrex store.rs).
	accountFkvSink := newBatchSink(db, cfIdxAccountFlatKeyValue)
	defer accountFkvSink.Close()
	storageFkvSink := newBatchSink(db, cfIdxStorageFlatKeyValue)
	defer storageFkvSink.Close()

	// Bytecode sinks: route account_codes + account_code_metadata through the
	// same batched, WAL-disabled path as the trie/flat-KV writes instead of
	// per-key db.put (which keeps the WAL on). Codes are ~10% of a
	// mainnet-shaped fill, so this avoids WAL writes + per-call WriteOptions
	// churn on a non-trivial slice of the import.
	codeSink := newBatchSink(db, cfIdxAccountCodes)
	defer codeSink.Close()
	codeMetaSink := newBatchSink(db, cfIdxAccountCodeMetadata)
	defer codeMetaSink.Close()

	stateRoot, stats, err := writeState(ctx, cfg, db, accountSink, storageSink, accountFkvSink, storageFkvSink, codeSink, codeMetaSink)
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

	// metadata.json must sit in the effective datadir (next to the RocksDB);
	// the genesis sidecar stays at the datadir root, where --network points.
	if err := WriteStoreMetadata(dbDir); err != nil {
		return nil, err
	}

	if err := WriteGenesisSidecar(cfg.DBPath, g); err != nil {
		return nil, err
	}

	stats.StateRoot = stateRoot
	return stats, nil
}
