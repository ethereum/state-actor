//go:build cgo_besu

package besu

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"

	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/besu/keys"
)

// writeGenesisBlock persists the genesis block (header / body / receipts /
// canonical-hash / total-difficulty in BLOCKCHAIN CF) and the chain head
// pointer in VARIABLES.
//
// chainHeadHash MUST be written LAST and with sync. Besu's
// DefaultBlockchain.setGenesis reads it as the boot gate: a partial run
// leaves the BLOCKCHAIN rows orphan but the gate absent → Besu refuses
// to boot, never silently mis-boots.
func (b *besuDB) writeGenesisBlock(header *types.Header, totalDifficulty *big.Int) error {
	blockHash := header.Hash()

	// Encode every payload up-front so encoder errors don't leave half-
	// written rows.
	headerRLP, err := gethrlp.EncodeToBytes(header)
	if err != nil {
		return fmt.Errorf("besu: encode header: %w", err)
	}

	// Genesis body is always 2-tuple [txs, ommers] per
	// GenesisState.buildGenesisBlock — withdrawals land in post-genesis
	// blocks even with shanghaiTime=0.
	bodyRLP, err := gethrlp.EncodeToBytes(struct {
		Txs    []*types.Transaction
		Ommers []*types.Header
	}{
		Txs:    []*types.Transaction{},
		Ommers: []*types.Header{},
	})
	if err != nil {
		return fmt.Errorf("besu: encode body: %w", err)
	}

	// Receipts: empty RLP list = single byte 0xC0. Direct write — NOT
	// RLP.encodeOne([]byte{0xC0}) which would double-wrap.
	receiptsRLP := []byte{0xC0}

	tdHash := common.BigToHash(totalDifficulty)

	if err := b.put(cfIdxBlockchain, keys.BlockHeaderKey(blockHash), headerRLP); err != nil {
		return fmt.Errorf("besu: write header: %w", err)
	}
	if err := b.put(cfIdxBlockchain, keys.BlockBodyKey(blockHash), bodyRLP); err != nil {
		return fmt.Errorf("besu: write body: %w", err)
	}
	if err := b.put(cfIdxBlockchain, keys.BlockReceiptsKey(blockHash), receiptsRLP); err != nil {
		return fmt.Errorf("besu: write receipts: %w", err)
	}
	if err := b.put(cfIdxBlockchain, keys.CanonicalHashKey(0), blockHash[:]); err != nil {
		return fmt.Errorf("besu: write canonical hash: %w", err)
	}
	if err := b.put(cfIdxBlockchain, keys.TotalDifficultyKey(blockHash), tdHash[:]); err != nil {
		return fmt.Errorf("besu: write total difficulty: %w", err)
	}

	// chainHeadHash LAST, synced — boot gate per setGenesis.
	if err := b.putSync(cfIdxVariables, keys.ChainHeadHashKey, blockHash[:]); err != nil {
		return fmt.Errorf("besu: write chainHeadHash: %w", err)
	}

	return nil
}

// supportedForkChainConfig was previously a Shanghai-ceiling gate; post-
// after the writer migration the besu writer goes through internal/genesisheader.Build
// which handles every fork up through Osaka. Function kept as a nil-
// input sanity check so callers still get a clear error if they pass
// an empty Genesis (would otherwise nil-deref later).
func supportedForkChainConfig(g *genesis.Genesis) error {
	if g == nil || g.Config == nil {
		return fmt.Errorf("besu: genesis chain config required")
	}
	return nil
}

// genesisJSONConfig is a minimal subset of the genesis JSON `config` block
// we need to detect unsupported fork activation. Larger fields (chainId,
// homesteadBlock, etc.) are loaded by the upstream genesis package; we
// only inspect post-Shanghai timestamps here.
type genesisJSONConfig struct {
	CancunTime *uint64 `json:"cancunTime,omitempty"`
	PragueTime *uint64 `json:"pragueTime,omitempty"`
}
