//go:build cgo_neth

// Real Run implementation behind the cgo_neth build tag. Only compiled
// inside the Dockerfile.nethermind build context where librocksdb and
// grocksdb are available.
//
// runImpl drives writer paths off generator.Config:
//
//   - Empty alloc (no --spec, no --target-size, no --genesis with non-zero
//     alloc): the seven RocksDBs get a block-0 row each with
//     WasProcessed=true so Nethermind's BlockTree boot detection skips its
//     own loader. State/Code stay empty (state root = EmptyTreeHash).
//   - Any non-empty input (auto-fill Plan from --target-size, spec
//     PreAlloc entities from --spec, or --genesis alloc): writeSyntheticAccounts
//     handles all three uniformly. Spec storage flows through the streaming
//     Phase 0; genesis alloc + auto-fill entities feed the addrHash-
//     sorted state-trie build via a temp Pebble.

package nethermind

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/genesisheader"
	"github.com/ethereum/state-actor/internal/neth"
)

// runImpl orchestrates a Nethermind RocksDB write.
//
// Pipeline:
//  1. Resolve genesis fields (gasLimit, extraData, timestamp, …) from
//     cfg.* / --genesis path.
//  2. Open 7 grocksdb instances directly under cfg.DBPath/ via openNethDBs
//     (which enforces the fresh-dir precondition).
//  3. Dispatch to writeSyntheticAccounts (handles synthetic generation,
//     genesis-alloc, AND spec-PreAlloc entities via the spec-storage
//     streaming Phase 0). Empty alloc + zero synthetic counts → state
//     stays empty and root = EmptyTreeHash.
//  4. Build the genesis header with the computed state root.
//  5. writeGenesisBlockToDBs assembles the 5 metadata DBs at block 0
//     (headers/blocks/blockNumbers/receipts, blockInfos LAST as the boot
//     gate; see genesis_cgo.go for the failure-window discipline).
//  6. Close cleanly and return Stats.
//
// opts is reserved for future boot-validation wiring; runImpl's body
// runs synchronously today.
func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = opts

	if cfg.TrieMode == generator.TrieModeBinary {
		return nil, errors.New("nethermind doesn't support binary trie (EIP-7864)")
	}
	if cfg.DBPath == "" {
		return nil, errors.New("--db is required for --client=nethermind")
	}

	// Pull genesis from cfg. Production callers (main.go) always set
	// this; tests can leave it nil and get the default chainspec via
	// genesis.OrDefault. Header construction below pulls every header
	// field from g via internal/genesisheader.Build.
	g := genesis.OrDefault(cfg.Genesis)

	dbs, err := openNethDBs(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open Nethermind DBs: %w", err)
	}
	defer dbs.Close()

	// State-trie population: writeSyntheticAccounts handles every non-
	// empty case (auto-fill Plan + genesis-alloc + spec-PreAlloc entities).
	// Its per-loop gates on plan.NumEOAs / plan.NumContracts collapse to
	// zero iterations when --spec is set without --target-size; the
	// genesis-alloc loop and the spec-storage streaming Phase 0 still
	// fire. Empty everything → state stays empty; root = EmptyTreeHash.
	stateRoot := common.Hash(neth.EmptyTreeHash)
	allocAccounts := cfg.GenesisAccounts
	allocCodes := cfg.GenesisCode
	allocStorages := cfg.GenesisStorage
	stats := &generator.Stats{}
	plannedEOAs, plannedContracts := 0, 0
	if cfg.AutoFill != nil {
		plannedEOAs = cfg.AutoFill.NumEOAs
		plannedContracts = cfg.AutoFill.NumContracts
	}
	if plannedEOAs > 0 || plannedContracts > 0 || len(allocAccounts) > 0 {
		stateRoot, err = writeSyntheticAccounts(ctx, dbs, cfg, allocAccounts, allocCodes, allocStorages, stats)
		if err != nil {
			return nil, fmt.Errorf("write state: %w", err)
		}
	}
	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes

	// Header construction now flows through internal/genesisheader.Build —
	// same path as besu/reth. Per-fork field activation
	// (BaseFee/WithdrawalsHash/ParentBeaconRoot/ExcessBlobGas/
	// BlobGasUsed/RequestsHash) is centralized there.
	header := genesisheader.Build(g, 0, common.Hash{}, stateRoot)

	hash, err := writeGenesisBlockToDBs(dbs, header)
	if err != nil {
		return nil, fmt.Errorf("write genesis: %w", err)
	}

	// Write Parity-style chainspec next to the DB so --chain-id is honoured
	// at boot (closes the B7 loop). Smoke scripts point Nethermind at this
	// via the Init config's ChainSpecPath.
	if _, err := writeChainSpec(cfg.DBPath, g, header.Root); err != nil {
		return nil, fmt.Errorf("nethermind: %w", err)
	}

	if cfg.Verbose {
		log.Printf("nethermind: genesis hash = %s", hash.Hex())
		log.Printf("nethermind: state root  = %s", header.Root.Hex())
		log.Printf("nethermind: 7 RocksDBs written under %s/", cfg.DBPath)
		if len(allocAccounts) > 0 {
			log.Printf("nethermind: preallocated %d accounts from cfg.GenesisAccounts (test-only path)", len(allocAccounts))
		}
		if plannedEOAs > 0 || plannedContracts > 0 {
			log.Printf("nethermind: synthesized %d EOAs + %d contracts (stats: %d / %d)",
				plannedEOAs, plannedContracts, stats.AccountsCreated, stats.ContractsCreated)
		}
	}

	stats.StateRoot = header.Root
	return stats, nil
}
