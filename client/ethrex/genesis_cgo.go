//go:build cgo_ethrex

package ethrex

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// writeGenesisBlock persists the genesis block rows to ethrex's RocksDB.
//
// Written CFs:
//   - headers[RLP(hash)]               = rlp.EncodeToBytes(header)
//   - bodies[RLP(hash)]                = 0xc3c0c0c0 (RLP [[],[],[]])
//   - block_numbers[RLP(hash)]         = u64-LE(0)
//   - chain_data[0x01]                 = u64-LE(0) (EarliestBlockNumber)
//   - chain_data[0x04]                 = u64-LE(0) (LatestBlockNumber)
//   - chain_data[0x80]                 = chainConfigJSON bytes (ChainConfig)
//   - canonical_block_hashes[u64-LE(0)]= RLP(hash)  ← boot gate, written LAST
//
// The canonical_block_hashes write MUST be the last durable write. ethrex's
// add_initial_state short-circuit checks canonical_block_hashes[0] → headers[hash];
// if the hash matches, it skips trie recomputation. A crash before this write
// leaves the DB unbootable, which is the desired loud-fail behavior.
func writeGenesisBlock(db *ethrexDB, header *types.Header, chainConfigJSON []byte) error {
	blockHash := header.Hash()

	// Pre-encode all values so encoder errors don't leave half-written rows.
	headerRLP, err := gethrlp.EncodeToBytes(header)
	if err != nil {
		return fmt.Errorf("ethrex: encode header: %w", err)
	}

	hashRLPKey, err := rlpEncodeHash(blockHash)
	if err != nil {
		return fmt.Errorf("ethrex: rlp encode block hash: %w", err)
	}

	// Genesis body: RLP([[],[],[]]) = 0xc3c0c0c0
	bodyRLP := []byte{0xc3, 0xc0, 0xc0, 0xc0}

	// Block number value: u64-LE(0)
	var blockNumLE [8]byte
	binary.LittleEndian.PutUint64(blockNumLE[:], 0)

	// Canonical block hashes key: u64-LE(0)
	var canonicalKey [8]byte
	binary.LittleEndian.PutUint64(canonicalKey[:], 0)

	// Write headers.
	if err := db.put(cfIdxHeaders, hashRLPKey, headerRLP); err != nil {
		return fmt.Errorf("ethrex: write header: %w", err)
	}

	// Write bodies.
	if err := db.put(cfIdxBodies, hashRLPKey, bodyRLP); err != nil {
		return fmt.Errorf("ethrex: write body: %w", err)
	}

	// Write block_numbers.
	if err := db.put(cfIdxBlockNumbers, hashRLPKey, blockNumLE[:]); err != nil {
		return fmt.Errorf("ethrex: write block_numbers: %w", err)
	}

	// Write chain_data.
	if err := db.put(cfIdxChainData, []byte{0x01}, blockNumLE[:]); err != nil {
		return fmt.Errorf("ethrex: write chain_data EarliestBlockNumber: %w", err)
	}
	if err := db.put(cfIdxChainData, []byte{0x04}, blockNumLE[:]); err != nil {
		return fmt.Errorf("ethrex: write chain_data LatestBlockNumber: %w", err)
	}
	if err := db.put(cfIdxChainData, []byte{0x80}, chainConfigJSON); err != nil {
		return fmt.Errorf("ethrex: write chain_data ChainConfig: %w", err)
	}

	// canonical_block_hashes — boot gate, written LAST with sync.
	if err := db.putSync(cfIdxCanonicalBlockHashes, canonicalKey[:], hashRLPKey); err != nil {
		return fmt.Errorf("ethrex: write canonical_block_hashes: %w", err)
	}

	return nil
}

// rlpEncodeHash returns 0xa0 || hash[:] — the RLP encoding of a 32-byte hash
// (an RLP byte string of length 32).
func rlpEncodeHash(h common.Hash) ([]byte, error) {
	return gethrlp.EncodeToBytes(h)
}
