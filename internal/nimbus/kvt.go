package nimbus

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
)

// KvtGen row keys (execution_chain/db/storage_types.nim). DbKey.toOpenArray
// uses an INCLUSIVE end position, so the singleton keys are two bytes long.
// Hash keys are kind ‖ hash32; u64 keys are kind ‖ u64 little-endian (the
// key is a memcpy of the native integer).
const (
	kindGenericHash       byte = 0
	kindBlockNumberToHash byte = 1
	kindBlockHashToScore  byte = 2
	kindCanonicalHeadHash byte = 4
	kindContractHash      byte = 6
	kindFcuNumAndHash     byte = 8
)

// FCU record indexes for KeyFcu (db/fcu_db.nim).
const (
	FcuHead      uint64 = 0
	FcuFinalized uint64 = 1
	FcuSafe      uint64 = 2
)

func hashKey(kind byte, h common.Hash) []byte {
	out := make([]byte, 33)
	out[0] = kind
	copy(out[1:], h[:])
	return out
}

func u64Key(kind byte, u uint64) []byte {
	out := make([]byte, 9)
	out[0] = kind
	binary.LittleEndian.PutUint64(out[1:], u)
	return out
}

// KeyHeader → RLP(Header) of the block with hash h.
func KeyHeader(h common.Hash) []byte { return hashKey(kindGenericHash, h) }

// KeyBlockNumToHash → RLPHash(canonical hash of block n).
func KeyBlockNumToHash(n uint64) []byte { return u64Key(kindBlockNumberToHash, n) }

// KeyScore → RLP(total difficulty as UInt256) of the block with hash h.
// persistHeader reads the parent's score, so block 1 cannot be imported
// without the genesis row.
func KeyScore(h common.Hash) []byte { return hashKey(kindBlockHashToScore, h) }

// KeyCanonicalHead → RLPHash(head). Its presence is what initializeDb treats
// as "database already initialised".
var KeyCanonicalHead = []byte{kindCanonicalHeadHash, 0x00}

// KeyCode → raw bytecode (not RLP) for codeHash. Never write an empty value:
// Kvt treats it as a delete.
func KeyCode(codeHash common.Hash) []byte { return hashKey(kindContractHash, codeHash) }

// KeyFcu → FcuValue for FcuHead / FcuFinalized / FcuSafe.
func KeyFcu(idx uint64) []byte { return u64Key(kindFcuNumAndHash, idx) }

// RLPHash is the RLP encoding of a 32-byte hash: 0xa0 ‖ h.
func RLPHash(h common.Hash) []byte {
	out := make([]byte, 33)
	out[0] = 0xa0
	copy(out[1:], h[:])
	return out
}

// RLPZeroScore is RLP(UInt256(0)), the total difficulty of a PoS genesis.
var RLPZeroScore = []byte{0x80}

// FcuValue is fcu_db.nim's record: BE64(number) ‖ hash (not RLP).
func FcuValue(number uint64, hash common.Hash) []byte {
	out := make([]byte, 40)
	binary.BigEndian.PutUint64(out[0:8], number)
	copy(out[8:], hash[:])
	return out
}
