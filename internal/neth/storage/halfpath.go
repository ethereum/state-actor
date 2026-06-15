// Package storage encodes the State DB keys Nethermind expects when reading
// trie nodes from the on-disk RocksDB.
//
// Two encodings are supported, mirroring Nethermind's
// `INodeStorage.KeyScheme`:
//
//   - HalfPath (default): 42-byte (state) or 74-byte (storage) keys that
//     embed both the path prefix and the node hash. Section byte at index
//     0 tags state-top / state-deep / storage so prefix scans don't have to
//     filter.
//   - Hash-only (fallback): 32-byte keys that are just the node's keccak hash.
//     Nethermind reads HalfPath first then falls back to Hash-only.
//
// We default to HalfPath (Nethermind's preferred encoding); Hash-only is
// available for fixtures or interop with older databases.
//
// Citation: src/Nethermind/Nethermind.Trie/NodeStorage.cs (lines 33-92 at
// upstream/master 09bd5a2d).
package storage

import (
	"github.com/ethereum/state-actor/internal/neth"
)

// StateNodeKeyLen is the byte length of a HalfPath state-trie node key:
// section(1) + path[:8] + pathLen(1) + keccak(32) = 42.
const StateNodeKeyLen = 42

// StorageNodeKeyLen is the byte length of a HalfPath storage-trie node key:
// section(1) + addrHash(32) + path[:8] + pathLen(1) + keccak(32) = 74.
const StorageNodeKeyLen = 74

// HashOnlyKeyLen is the byte length of a Hash-only fallback key: just the
// 32-byte keccak hash. Nethermind also defines `StoragePathLength = 74` as
// the upper bound for the path span; the hash-only encoder writes only 32
// bytes into that span.
const HashOnlyKeyLen = 32

// StateNodeKey returns the HalfPath-encoded key for a state-trie node.
//
// Layout (matches NodeStorage.cs:GetHalfPathNodeStoragePathSpan, address-nil branch):
//
//	out[0]      section byte: 0 if pathLen <= TopStateBoundary, else 1
//	out[1..9]   first 8 bytes of pathBytes (the byte-packed nibble representation)
//	out[9]      pathLen as a single byte
//	out[10..42] keccak hash of the node's RLP
//
// pathBytes MUST be at least 8 bytes long. Callers typically pass the
// 32-byte packed path representation (the same representation
// Nethermind's `TreePath.Path.BytesAsSpan` exposes); only the first 8
// bytes are read. pathLen is the count of nibbles in the underlying path
// (0-64), independent of pathBytes length.
//
// keccak is the keccak256 of the node's RLP. EmptyTreeHash should never
// reach this function — Nethermind short-circuits reads at that hash.
func StateNodeKey(pathBytes []byte, pathLen int, keccak [32]byte) []byte {
	if len(pathBytes) < 8 {
		panic("storage.StateNodeKey: pathBytes must be at least 8 bytes")
	}
	out := make([]byte, StateNodeKeyLen)
	if pathLen <= neth.TopStateBoundary {
		out[0] = 0
	} else {
		out[0] = 1
	}
	copy(out[1:9], pathBytes[:8])
	out[9] = byte(pathLen)
	copy(out[10:42], keccak[:])
	return out
}

// StorageNodeKey returns the HalfPath-encoded key for a storage-trie node.
//
// Layout (matches NodeStorage.cs:GetHalfPathNodeStoragePathSpan, address-set branch):
//
//	out[0]       section byte: always 2
//	out[1..33]   addrHash (keccak256(address))
//	out[33..41]  first 8 bytes of pathBytes
//	out[41]      pathLen as a single byte
//	out[42..74]  keccak hash of the node's RLP
func StorageNodeKey(addrHash [32]byte, pathBytes []byte, pathLen int, keccak [32]byte) []byte {
	if len(pathBytes) < 8 {
		panic("storage.StorageNodeKey: pathBytes must be at least 8 bytes")
	}
	out := make([]byte, StorageNodeKeyLen)
	out[0] = 2
	copy(out[1:33], addrHash[:])
	copy(out[33:41], pathBytes[:8])
	out[41] = byte(pathLen)
	copy(out[42:74], keccak[:])
	return out
}

// HashOnlyKey returns the Hash-only fallback key (just the 32-byte keccak).
// Nethermind reads this only after a HalfPath miss, so writing in this form
// works but is slower than HalfPath for the same on-disk payload.
func HashOnlyKey(keccak [32]byte) []byte {
	out := make([]byte, HashOnlyKeyLen)
	copy(out, keccak[:])
	return out
}
