package ethrex

import "github.com/ethereum/go-ethereum/common"

// StoragePrefix returns the 66-nibble storage-trie key prefix for the given
// address hash. The prefix is:
//
//	BytesToNibbles(addrHash[:])  — 64 nibbles
//	++ [LeafFlag]                — nibble 16
//	++ [StorageSeparator]        — nibble 17
//
// Mirrors ethrex layering.rs:300-307: Nibbles::from_bytes(addr) (which appends
// the leaf flag 16 via from_bytes), then .append_new(17).
//
// The storage-trie ROOT row is keyed by exactly this 66-nibble prefix.
func StoragePrefix(addrHash common.Hash) []byte {
	nibs := BytesToNibbles(addrHash[:]) // 64 nibbles
	prefix := make([]byte, 64+1+1)
	copy(prefix, nibs)
	prefix[64] = LeafFlag
	prefix[65] = StorageSeparator
	return prefix
}

// PrefixedSink returns a NodeSink that prepends StoragePrefix(addrHash) to
// every path before forwarding to the underlying sink. Use this to route a
// per-account storage Builder's raw node paths into storage_trie_nodes.
//
// The Builder emits raw (unprefixed) paths. This wrapper applies the 66-nibble
// address prefix so that the emitted (key, value) pairs are ready to write
// directly into the storage_trie_nodes column family.
//
// Mirrors ethrex layering.rs:300-307 apply_prefix usage in store.rs
// setup_genesis_state_trie.
func PrefixedSink(addrHash common.Hash, sink NodeSink) NodeSink {
	prefix := StoragePrefix(addrHash)
	return func(pathNibbles []byte, value []byte) error {
		fullKey := make([]byte, len(prefix)+len(pathNibbles))
		copy(fullKey, prefix)
		copy(fullKey[len(prefix):], pathNibbles)
		return sink(fullKey, value)
	}
}
