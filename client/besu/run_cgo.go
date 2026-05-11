//go:build cgo_besu

package besu

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/genesisheader"
)

// runImpl is the cgo_besu orchestrator. The on-disk write order is the
// load-bearing invariant: any earlier-step failure must leave the DB in a
// state that Besu refuses to boot, so a partial run can't be mistaken for
// a complete one.
//
//   - DB opens before metadata writes (fresh-dir guard mustn't leave orphan
//     metadata claiming an intact DB exists).
//   - Within writeGenesisBlock, chainHeadHash writes LAST with sync — it's
//     the boot gate per DefaultBlockchain.setGenesis.
//   - DATABASE_METADATA.json is the last on-disk write; missing it makes
//     Besu fatal with StorageException, which is the desired loud-fail.
func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("besu: --db is required")
	}
	// Besu reads chainId from --genesis-file at boot, not from the DB.
	// B7 (chainID embedding via state-actor-written chainspec) is the
	// follow-up that makes --chain-id take effect end-to-end.
	//
	// Tests can leave cfg.Genesis nil; synthesize an Osaka default
	// (besu's writer ceiling after the writer migration to internal/genesisheader.Build — see
	// genesis.MaxForkForClient("besu")). Header construction goes
	// through internal/genesisheader.Build, which handles every fork
	// up through Osaka uniformly across all 4 clients.
	g := cfg.Genesis
	if g == nil {
		g, _ = genesis.BuildSynthetic("osaka", nil, 0, 0, nil)
	}
	if g.Config == nil {
		return nil, errors.New("besu: cfg.Genesis must have Config set (use genesis.BuildSynthetic)")
	}

	db, err := openBesuDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	sink := newNodeSink(db)
	defer sink.Close()

	rootHash, rootRLP, stats, err := writeStateAndCollectRoot(ctx, cfg, db, sink)
	if err != nil {
		return nil, err
	}

	header := genesisheader.Build(g, 0, common.Hash{}, rootHash)
	if err := sink.SaveWorldState(header.Hash(), rootHash, rootRLP); err != nil {
		return nil, err
	}
	if err := db.writeAdvisorySentinels(); err != nil {
		return nil, err
	}
	totalDifficulty := big.NewInt(0)
	if g.Difficulty != nil {
		totalDifficulty = g.Difficulty.ToInt()
	}
	if err := db.writeGenesisBlock(header, totalDifficulty); err != nil {
		return nil, err
	}
	if err := WriteDatabaseMetadata(cfg.DBPath); err != nil {
		return nil, err
	}

	// Write besu-bootable chainspec next to the DB so the chainID the
	// caller passed via --chain-id is honoured at boot time. Smoke
	// scripts pass --genesis-file=<dbPath>/besu-chainspec.json.
	if _, err := writeChainSpec(cfg.DBPath, g); err != nil {
		return nil, fmt.Errorf("besu: %w", err)
	}

	stats.StateRoot = rootHash
	return stats, nil
}

// Header construction now flows through internal/genesisheader.Build
// — same path as geth/reth/(after the writer migration to internal/genesisheader.Build) nethermind. Per-fork field
// activation (BaseFee/WithdrawalsHash/ParentBeaconRoot/ExcessBlobGas/
// BlobGasUsed/RequestsHash) is centralized there. The previous hand-
// rolled *besuGenesis struct + buildGenesisHeader func are gone.
