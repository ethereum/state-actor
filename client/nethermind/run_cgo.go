//go:build cgo_neth

// Real Run implementation behind the cgo_neth build tag. Only compiled
// inside the Dockerfile.nethermind build context where librocksdb-dev
// and grocksdb are available.
//
// Phase A (this commit): empty-alloc genesis only. State and Code DBs
// stay empty; the 5 metadata DBs (blocks, headers, blockNumbers,
// blockInfos, receipts) get block 0 entries with WasProcessed=true so
// Nethermind's BlockTree boot detection skips its own loader.
//
// Phase B (next commit on this branch): wire entitygen.Source
// → internal/neth/trie.Builder → grocksdb writes for State/Code DBs so
// the writer scales to a multi-million-account devnet.

package nethermind

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/neth"
)

// runImpl orchestrates a Nethermind RocksDB write.
//
// Phase A pipeline:
//  1. Resolve genesis fields (chain ID, gasLimit, extraData, timestamp)
//     from cfg.* / --genesis path.
//  2. Open 7 grocksdb instances directly under cfg.DBPath/.
//  3. Build the empty-alloc genesis header (state root = EmptyTreeHash).
//  4. Write the genesis row across blocks/, headers/, blockNumbers/,
//     blockInfos/ (with WasProcessed=true), receipts/Blocks CF.
//  5. Close cleanly. Return Stats with the computed state root and the
//     genesis block hash for visibility.
func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = ctx
	_ = opts

	if cfg.TrieMode == generator.TrieModeBinary {
		return nil, errors.New("nethermind doesn't support binary trie (EIP-7864)")
	}
	if cfg.DBPath == "" {
		return nil, errors.New("--db is required for --client=nethermind")
	}

	// Deep-branch mode is binary-trie-only and not supported on Nethermind.
	if cfg.DeepBranch.Enabled() {
		return nil, errors.New("--client=nethermind: --deep-branch-* flags are MPT-incompatible")
	}

	// Pull genesis fields. If the caller passed --genesis, use those values;
	// otherwise default to a dev-mode-ish minimal genesis (chainId 1337,
	// gasLimit 30M, empty extraData, timestamp 0).
	chainID := int64(1337)
	gasLimit := uint64(30_000_000)
	var extraData []byte
	var timestamp uint64
	var loadedGenesis *genesis.Genesis
	if GenesisFilePath != "" {
		g, err := genesis.LoadGenesis(GenesisFilePath)
		if err != nil {
			return nil, fmt.Errorf("load genesis: %w", err)
		}
		loadedGenesis = g
		if g.Config != nil && g.Config.ChainID != nil {
			chainID = g.Config.ChainID.Int64()
		}
		if g.GasLimit != 0 {
			gasLimit = uint64(g.GasLimit)
		}
		if len(g.ExtraData) > 0 {
			extraData = g.ExtraData
		}
		timestamp = uint64(g.Timestamp)
	}
	if ChainIDOverride != 0 {
		chainID = ChainIDOverride
		// state-actor writes nothing that carries chainID: the genesis
		// header has no chainID field, the state trie hashes are
		// chainID-blind, and the genesis block has zero txs (so no
		// EIP-155 fingerprints). chainID lives entirely in the
		// chainspec Nethermind reads on boot — same DB can be served
		// under any chainID without regeneration. Surface the
		// no-effect-here behavior so users don't expect otherwise.
		log.Printf("nethermind: --chain-id=%d is parsed but not embedded in the produced DB "+
			"(chainID lives in the chainspec at boot time; the same DB serves any chainID). "+
			"Set the chainID in your Nethermind chainspec/config instead.", chainID)
	}
	_ = chainID

	dbs, err := openNethDBs(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open Nethermind DBs: %w", err)
	}
	defer dbs.Close()

	// State-trie population: dispatch by what the caller asked for.
	//   - synthetic --accounts=N or --contracts=N → writeSyntheticAccounts
	//     (entitygen → temp Pebble → addrHash-sorted state trie). It also
	//     folds in genesis-alloc accounts so the same sort produces a
	//     unified root.
	//   - genesis-alloc only (no synthetic) → writeGenesisAllocAccounts (a
	//     simpler in-memory path without the temp Pebble round-trip).
	//   - empty alloc → state stays empty; root = EmptyTreeHash.
	stateRoot := common.Hash(neth.EmptyTreeHash)
	var allocAccounts map[common.Address]*types.StateAccount
	var allocCodes map[common.Address][]byte
	var allocStorages map[common.Address]map[common.Hash]common.Hash
	if loadedGenesis != nil && len(loadedGenesis.Alloc) > 0 {
		allocAccounts = loadedGenesis.ToStateAccounts()
		allocCodes = loadedGenesis.GetAllocCode()
		allocStorages = loadedGenesis.GetAllocStorage()
	}
	switch {
	case cfg.NumAccounts > 0 || cfg.NumContracts > 0:
		// writeSyntheticAccounts doesn't yet thread genesis-alloc storage
		// through the temp-Pebble pipeline. If the user supplied a --genesis
		// whose alloc carries non-empty storage AND asked for synthetic
		// accounts, those storage slots would silently disappear from the
		// state root we write. Fail loud until storage threading lands —
		// tracked at https://github.com/nerolation/state-actor/issues/22.
		if len(allocStorages) > 0 {
			return nil, fmt.Errorf(
				"--client=nethermind: --genesis with %d storage-bearing alloc account(s) is "+
					"incompatible with --accounts/--contracts > 0. The synthetic-accounts path "+
					"does not yet write genesis-alloc storage tries; using it now would silently "+
					"drop your storage entries. Run again without --accounts/--contracts to use "+
					"the genesis-alloc-only path, or remove storage from the alloc. "+
					"Tracked at https://github.com/nerolation/state-actor/issues/22.",
				len(allocStorages),
			)
		}
		stateRoot, err = writeSyntheticAccounts(dbs, cfg, allocAccounts, allocCodes)
		if err != nil {
			return nil, fmt.Errorf("write synthetic accounts: %w", err)
		}
	case len(allocAccounts) > 0:
		stateRoot, err = writeGenesisAllocAccounts(dbs, allocAccounts, allocCodes, allocStorages)
		if err != nil {
			return nil, fmt.Errorf("write genesis alloc: %w", err)
		}
	}

	header := buildEmptyAllocGenesisHeader(chainID, gasLimit, extraData, timestamp)
	header.Root = stateRoot

	hash, err := writeGenesisBlockToDBs(dbs, header)
	if err != nil {
		return nil, fmt.Errorf("write genesis: %w", err)
	}

	if cfg.Verbose {
		log.Printf("nethermind: genesis hash = %s", hash.Hex())
		log.Printf("nethermind: state root  = %s", header.Root.Hex())
		log.Printf("nethermind: 7 RocksDBs written under %s/", cfg.DBPath)
		if loadedGenesis != nil && len(loadedGenesis.Alloc) > 0 {
			log.Printf("nethermind: preallocated %d accounts from --genesis", len(loadedGenesis.Alloc))
		}
		if cfg.NumAccounts > 0 || cfg.NumContracts > 0 {
			log.Printf("nethermind: synthesized %d EOAs + %d contracts", cfg.NumAccounts, cfg.NumContracts)
		}
	}

	return &generator.Stats{
		StateRoot:        header.Root,
		AccountsCreated:  cfg.NumAccounts,
		ContractsCreated: cfg.NumContracts,
	}, nil
}

// GenesisFilePath / ChainIDOverride are declared in run.go (no build
// tag) so main.go's assignments compile in both build modes. The cgo
// build path reads them from runImpl above; the stub path ignores them.
