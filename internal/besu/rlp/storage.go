package rlp

import (
	"github.com/ethereum/go-ethereum/common"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// TrimStorageValue returns the leading-zero-trimmed big-endian bytes of a
// 32-byte storage slot value. This is the encoding Besu's Bonsai flat-db
// uses for the ACCOUNT_STORAGE_STORAGE column family — values are read
// back via UInt256.fromBytes which rejects > 32 bytes, so flat-db must
// NEVER carry the RLP wrapper.
//
// Mirrors the call at BonsaiWorldState.java:182 →
// putStorageValueBySlotHash(addrHash, slotHash, updatedStorage) where
// `updatedStorage` is the raw trimmed bytes (NOT encodeTrieValue).
//
// Edge cases:
//   - All-zero value: returns empty slice (zero slots are typically not
//     written at all; callers should check before persisting).
//   - Single non-zero byte: returns 1-byte slice.
//   - 32 non-zero bytes: returns the full 32-byte slice.
func TrimStorageValue(value common.Hash) []byte {
	raw := value[:]
	start := 0
	for start < len(raw) && raw[start] == 0x00 {
		start++
	}
	return raw[start:]
}

// EncodeStorageValue RLP-encodes a 32-byte storage slot value for the
// trie-side write path. Mirrors PathBasedWorldView.encodeTrieValue
// (PathBasedWorldView.java:43-47): trim leading zeros then RLP-encode as
// a byte string. The trie's hash function reads this exact encoding when
// computing nodes, so the storage trie root matches what Besu would
// compute for the same slot set.
//
// Edge cases:
//   - Zero value (all 32 bytes = 0x00): trimmed = []byte{}, RLP([]) = 0x80.
//   - Single non-zero byte x where x <= 0x7f: RLP = byte x itself (self-encoded).
//   - Single non-zero byte x where x > 0x7f: RLP = 0x81 ++ x.
//   - N-byte value (N > 1): RLP = (0x80+N) ++ bytes.
//
// Do NOT use this for flat-db writes — see TrimStorageValue.
func EncodeStorageValue(value common.Hash) []byte {
	trimmed := TrimStorageValue(value)
	encoded, err := gethrlp.EncodeToBytes(trimmed)
	if err != nil {
		// gethrlp.EncodeToBytes on a []byte never returns an error.
		panic("besu/rlp.EncodeStorageValue: unexpected RLP error: " + err.Error())
	}
	return encoded
}
