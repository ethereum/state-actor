//go:build cgo_nimbus

package nimbus

import (
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"

	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

// writeGenesisBlock persists the genesis block rows to KvtGen, in the order
// nimbus's own persistHeaderAndSetHead + fcuHead write them:
//
//	0x00‖hash        → RLP(header)
//	0x02‖hash        → RLP(total difficulty)   (= header.Difficulty at genesis)
//	0x01‖LE64(0)     → 0xa0‖hash
//	0x08‖LE64(0)     → BE64(0)‖hash           (fcuHead)
//	0x04 0x00        → 0xa0‖hash               (canonical head — written LAST, sync)
//
// The canonical-head row is the presence check initializeDb uses; a crash
// before it leaves nimbus re-running its own genesis init (which would fail
// loudly on the non-empty state), which is the desired behaviour. The data CFs
// are flushed to SST first: the bulk vertex and code rows skipped the WAL, so
// without the flush the gate could survive a crash that the state did not.
func writeGenesisBlock(db *nimbusDB, header *types.Header) error {
	hash := header.Hash()
	headerRLP, err := gethrlp.EncodeToBytes(header)
	if err != nil {
		return fmt.Errorf("nimbus: encode header: %w", err)
	}
	score, err := gethrlp.EncodeToBytes(header.Difficulty)
	if err != nil {
		return fmt.Errorf("nimbus: encode score: %w", err)
	}
	rlpHash := nimbusinternal.RLPHash(hash)

	if err := db.put(cfIdxKvtGen, nimbusinternal.KeyHeader(hash), headerRLP); err != nil {
		return fmt.Errorf("nimbus: write header: %w", err)
	}
	if err := db.put(cfIdxKvtGen, nimbusinternal.KeyScore(hash), score); err != nil {
		return fmt.Errorf("nimbus: write score: %w", err)
	}
	if err := db.put(cfIdxKvtGen, nimbusinternal.KeyBlockNumToHash(0), rlpHash); err != nil {
		return fmt.Errorf("nimbus: write block-number → hash: %w", err)
	}
	if err := db.put(cfIdxKvtGen, nimbusinternal.KeyFcu(nimbusinternal.FcuHead), nimbusinternal.FcuValue(0, hash)); err != nil {
		return fmt.Errorf("nimbus: write fcuHead: %w", err)
	}
	if err := db.flushData(); err != nil {
		return fmt.Errorf("nimbus: flush state before canonical head: %w", err)
	}
	if err := db.putSync(cfIdxKvtGen, nimbusinternal.KeyCanonicalHead, rlpHash); err != nil {
		return fmt.Errorf("nimbus: write canonical head: %w", err)
	}
	return nil
}
