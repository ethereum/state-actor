//go:build cgo_besu

package besu

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
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
	// Tests can leave cfg.Genesis nil; we synthesize a Shanghai default
	// (besu's writer ceiling — see genesis.MaxForkForClient("besu")) so
	// the supportedForkChainConfig gate doesn't reject the global default.
	g := cfg.Genesis
	if g == nil {
		g, _ = genesis.BuildSynthetic("shanghai", nil, 0, 0, nil)
	}
	genesisCfg := besuGenesisFromConfig(g)
	if err := supportedForkChainConfig(g); err != nil {
		return nil, err
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

	header := buildGenesisHeader(genesisCfg, rootHash)
	if err := sink.SaveWorldState(header.Hash(), rootHash, rootRLP); err != nil {
		return nil, err
	}
	if err := db.writeAdvisorySentinels(); err != nil {
		return nil, err
	}
	if err := db.writeGenesisBlock(header, genesisCfg.difficulty); err != nil {
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

// besuGenesis holds the header fields besu's writer needs. Built from
// the in-memory cfg.Genesis (synthesized by genesis.BuildSynthetic in
// main.go); no JSON re-parsing required.
type besuGenesis struct {
	chainID    *big.Int
	gasLimit   uint64
	difficulty *big.Int
	timestamp  uint64
	extraData  []byte
	coinbase   common.Address
	mixHash    common.Hash
	parentHash common.Hash
	nonce      uint64
	baseFee    *big.Int // nil if pre-London; *big.Int if londonBlock <= 0
}

// besuGenesisFromConfig translates an in-memory *genesis.Genesis into the
// flat besuGenesis struct the writer consumes.
func besuGenesisFromConfig(g *genesis.Genesis) *besuGenesis {
	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}
	diff := big.NewInt(0)
	if g.Difficulty != nil {
		diff = g.Difficulty.ToInt()
	}
	chainID := big.NewInt(1337)
	if g.Config != nil && g.Config.ChainID != nil {
		chainID = g.Config.ChainID
	}
	var baseFee *big.Int
	if g.BaseFee != nil {
		baseFee = g.BaseFee.ToInt()
	}
	return &besuGenesis{
		chainID:    chainID,
		gasLimit:   gasLimit,
		difficulty: diff,
		timestamp:  uint64(g.Timestamp),
		extraData:  []byte(g.ExtraData),
		coinbase:   g.Coinbase,
		mixHash:    g.Mixhash,
		parentHash: g.ParentHash,
		nonce:      uint64(g.Nonce),
		baseFee:    baseFee,
	}
}

// buildGenesisHeader assembles a *types.Header for the genesis block from
// the besuGenesis fields plus the computed state root.
//
// Empty-list / empty-trie fields (TxHash, ReceiptHash, UncleHash, Bloom,
// WithdrawalsHash if Shanghai+) use the canonical Ethereum constants so
// the resulting block hash matches what Besu's GenesisState.buildHeader
// would compute for the same alloc.
func buildGenesisHeader(g *besuGenesis, stateRoot common.Hash) *types.Header {
	// EmptyTrie is the standard Ethereum MPT root for empty txs/receipts.
	emptyTrie := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	// Empty list keccak — used as the OmmersHash for genesis.
	emptyList := common.HexToHash("0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347")

	header := &types.Header{
		ParentHash:  g.parentHash,
		UncleHash:   emptyList,
		Coinbase:    g.coinbase,
		Root:        stateRoot,
		TxHash:      emptyTrie,
		ReceiptHash: emptyTrie,
		Bloom:       types.Bloom{},
		Difficulty:  g.difficulty,
		Number:      big.NewInt(0),
		GasLimit:    g.gasLimit,
		GasUsed:     0,
		Time:        g.timestamp,
		Extra:       g.extraData,
		MixDigest:   g.mixHash,
		Nonce:       types.EncodeNonce(g.nonce),
		BaseFee:     g.baseFee, // nil if pre-London; *big.Int if londonBlock=0
	}
	return header
}
