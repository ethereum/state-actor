//go:build cgo_neth

// Real Run implementation behind the cgo_neth build tag. Only compiled
// inside the Dockerfile.nethermind build context where librocksdb and
// grocksdb are available.
//
// runImpl drives three writer paths off generator.Config:
//
//   - Empty alloc (no --accounts/--contracts, no --genesis with non-zero
//     alloc): the seven RocksDBs get a block-0 row each with
//     WasProcessed=true so Nethermind's BlockTree boot detection skips its
//     own loader. State/Code stay empty (state root = EmptyTreeHash).
//   - Genesis-alloc only (--genesis JSON with alloc, no synthetic
//     accounts): writeGenesisAllocAccounts walks the alloc, writes
//     accounts + storage tries + code, returns the computed state root.
//   - Synthetic + optional genesis-alloc (--accounts/--contracts > 0):
//     writeSyntheticAccounts streams entitygen-generated entities through
//     a temp Pebble for sorted-by-addrHash account-trie build. Storage
//     trees for genesis-alloc accounts are not yet threaded into this
//     path; runImpl fails loud if the user combines them. Tracked at
//     https://github.com/nerolation/state-actor/issues/22.

package nethermind

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/neth"
)

// runImpl orchestrates a Nethermind RocksDB write.
//
// Pipeline:
//  1. Resolve genesis fields (gasLimit, extraData, timestamp, …) from
//     cfg.* / --genesis path.
//  2. Open 7 grocksdb instances directly under cfg.DBPath/ via openNethDBs
//     (which enforces the fresh-dir precondition).
//  3. Dispatch to writeSyntheticAccounts / writeGenesisAllocAccounts /
//     empty-alloc to populate the State + Code DBs and compute the state
//     root.
//  4. Build the genesis header with the computed state root.
//  5. writeGenesisBlockToDBs assembles the 5 metadata DBs at block 0
//     (headers/blocks/blockNumbers/receipts, blockInfos LAST as the boot
//     gate; see genesis_cgo.go for the failure-window discipline).
//  6. Close cleanly and return Stats.
//
// ctx and opts are reserved for future cancellation / boot-validation
// wiring; runImpl's body runs synchronously today.
func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = ctx
	_ = opts

	if cfg.TrieMode == generator.TrieModeBinary {
		return nil, errors.New("nethermind doesn't support binary trie (EIP-7864)")
	}
	if cfg.DBPath == "" {
		return nil, errors.New("--db is required for --client=nethermind")
	}

	// Pull genesis fields from cfg.Genesis. Production callers (main.go)
	// always set this; tests can leave it nil and get the default chainspec.
	g := genesis.OrDefault(cfg.Genesis)
	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}

	// Pull genesis fields from cfg.Genesis. Production callers (main.go)
	// always set this; tests can leave it nil and get the default chainspec.
	g := genesis.OrDefault(cfg.Genesis)
	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}
	extraData := []byte(g.ExtraData)
	timestamp := uint64(g.Timestamp)
	// chainID embedding is a B7 follow-up: nethermind reads chainID from
	// the chainspec at boot, not the on-disk DB. Until state-actor writes
	// a Parity chainspec under cfg.DBPath, the supplied chainID is recorded
	// here for documentation and stays inert.
	_ = g.Config.ChainID

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
	// alloc walking is kept for tests (cfg.GenesisAccounts/Storage/Code).
	// The CLI never sets these — production runs always go through
	// writeSyntheticAccounts with empty alloc.
	stateRoot := common.Hash(neth.EmptyTreeHash)
	allocAccounts := cfg.GenesisAccounts
	allocCodes := cfg.GenesisCode
	allocStorages := cfg.GenesisStorage
	switch {
	case cfg.NumAccounts > 0 || cfg.NumContracts > 0:
		// writeSyntheticAccounts doesn't yet thread alloc storage through
		// the temp-Pebble pipeline. If a test supplies cfg.GenesisStorage
		// AND asks for synthetic accounts, those storage slots would
		// silently disappear from the state root. Fail loud — tracked at
		// https://github.com/nerolation/state-actor/issues/22.
		if len(allocStorages) > 0 {
			return nil, fmt.Errorf(
				"--client=nethermind: cfg.GenesisStorage with %d storage-bearing alloc account(s) "+
					"is incompatible with --accounts/--contracts > 0. The synthetic-accounts path "+
					"does not yet write alloc storage tries; using it now would silently drop your "+
					"storage entries. Run with --accounts=0 --contracts=0 to use the alloc-only path, "+
					"or remove storage from the alloc. Tracked at "+
					"https://github.com/nerolation/state-actor/issues/22.",
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

	header := buildEmptyAllocGenesisHeader(g.Config.ChainID.Int64(), gasLimit, extraData, timestamp)
	header.Root = stateRoot

	hash, err := writeGenesisBlockToDBs(dbs, header)
	if err != nil {
		return nil, fmt.Errorf("write genesis: %w", err)
	}

	// Write Parity-style chainspec next to the DB so --chain-id is honoured
	// at boot (closes the B7 loop). Smoke scripts point Nethermind at this
	// via the Init config's ChainSpecPath.
	if _, err := writeChainSpec(cfg.DBPath, g); err != nil {
		return nil, fmt.Errorf("nethermind: %w", err)
	}

	if cfg.Verbose {
		log.Printf("nethermind: genesis hash = %s", hash.Hex())
		log.Printf("nethermind: state root  = %s", header.Root.Hex())
		log.Printf("nethermind: 7 RocksDBs written under %s/", cfg.DBPath)
		if len(allocAccounts) > 0 {
			log.Printf("nethermind: preallocated %d accounts from cfg.GenesisAccounts (test-only path)", len(allocAccounts))
		}
		if cfg.NumAccounts > 0 || cfg.NumContracts > 0 {
			log.Printf("nethermind: synthesized %d EOAs + %d contracts", cfg.NumAccounts, cfg.NumContracts)
		}
	}

	return &generator.Stats{
		StateRoot:        header.Root,
		AccountsCreated:  cfg.NumAccounts + len(cfg.InjectAddresses),
		ContractsCreated: cfg.NumContracts,
	}, nil
}

