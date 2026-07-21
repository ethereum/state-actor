package rlp

import (
	"github.com/ethereum/go-ethereum/common"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// EncodeStorageValue RLP-encodes a storage slot value with its leading zero
// bytes trimmed — the wire format Nethermind stores both as the storage-trie
// leaf value and (1.39.0+) as the flat Storage CF value. Returns nil for the
// all-zero value, which represents a deletion in MPT semantics and an absent
// row in the flat Storage CF.
//
// Mirrors the trimmed-then-RLP encoding Nethermind applies at both sites:
// Nethermind.State/StorageTree.cs (trie leaf) and
// Nethermind.State.Flat/Persistence/BaseFlatPersistence.cs SetStorage
// (RLP-wrapped flat slot) @ tag 1.39.0. It is byte-identical to what the
// Nethermind writer previously computed inline (encodeStorageValueNeth).
func EncodeStorageValue(value common.Hash) ([]byte, error) {
	v := value[:]
	for len(v) > 0 && v[0] == 0 {
		v = v[1:]
	}
	if len(v) == 0 {
		return nil, nil
	}
	return gethrlp.EncodeToBytes(v)
}
