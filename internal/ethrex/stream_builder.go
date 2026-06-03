package ethrex

import "github.com/ethereum/go-ethereum/common"

// StreamHashBuilder adapts the streaming MPT Builder to the
// streamingtrie.HashBuilder contract (AddLeaf(keyHash, valueRLP) + Root), so
// the ethrex storage feed can be driven by internal/streamingtrie's disk-backed
// streamsort drain — exactly as reth's storage writer does
// (client/reth/spec_storage_streaming_cgo.go rethStorageHashBuilder).
//
// streamingtrie calls AddLeaf with the 32-byte keccak key hash and the
// RLP-encoded leaf value, in strictly keccak-ascending order — which is the
// order Builder.AddLeaf requires. The only adaptation is expanding the 32-byte
// hash to its 64 nibbles before delegating.
type StreamHashBuilder struct {
	b *Builder
}

// NewStreamHashBuilder returns a StreamHashBuilder emitting rows via sink.
func NewStreamHashBuilder(sink NodeSink) *StreamHashBuilder {
	return &StreamHashBuilder{b: NewBuilder(sink)}
}

// AddLeaf inserts a (keyHash, valueRLP) pair, expanding the hash to nibbles.
func (s *StreamHashBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return s.b.AddLeaf(BytesToNibbles(keyHash[:]), valueRLP)
}

// Root finalizes the trie and returns the root hash.
func (s *StreamHashBuilder) Root() (common.Hash, error) {
	return s.b.Root()
}

// SuppressEmptyTrieSentinel wraps sink to drop the empty-trie sentinel row
// ([] -> 0x80) that Builder.Root() emits for a leafless trie. A storage writer
// that drives the Builder through a prefixing sink must NOT persist a
// (prefix, 0x80) row for a contract whose storage drained to zero non-zero
// slots — the pre-streaming code wrote zero storage rows in that case.
//
// The sentinel is unambiguous: a non-empty trie's root row is ([], <node RLP>)
// where the node RLP is a multi-byte list, never the single byte 0x80, so no
// real row is dropped. The returned root is unaffected (Root still returns
// EMPTY_TRIE_HASH for the empty case).
func SuppressEmptyTrieSentinel(sink NodeSink) NodeSink {
	return func(path, value []byte) error {
		if len(path) == 0 && len(value) == 1 && value[0] == 0x80 {
			return nil
		}
		return sink(path, value)
	}
}
