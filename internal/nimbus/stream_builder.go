package nimbus

import (
	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/internal/ethrex"
)

// StreamHashBuilder adapts Builder to internal/streamingtrie's HashBuilder
// contract (AddLeaf(keyHash, valueRLP) in keccak-ascending order, then Root)
// for storage tries fed by a spec entity's slot iterator: the 32-byte key
// hash expands to 64 nibbles and the storage record payload is derived from
// the value RLP. Unlike ethrex's adapter it needs no empty-trie suppression —
// Builder emits nothing for a leafless trie.
type StreamHashBuilder struct {
	b       *Builder
	nib     [64]byte
	payload [40]byte
}

// NewStreamHashBuilder returns an adapter over NewBuilder(root, alloc, sink).
func NewStreamHashBuilder(root VertexID, alloc *VidAllocator, sink Sink) *StreamHashBuilder {
	return &StreamHashBuilder{b: NewBuilder(root, alloc, sink)}
}

// AddLeaf inserts one storage slot.
func (s *StreamHashBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return s.b.AddLeaf(
		ethrex.AppendNibbles(s.nib[:0], keyHash[:]),
		valueRLP,
		AppendStoLeafPayloadFromRLP(s.payload[:0], valueRLP),
	)
}

// Root finalizes the trie and returns the storage root.
func (s *StreamHashBuilder) Root() (common.Hash, error) { return s.b.Root() }

// StoHint is Builder.StoHint (call after Root).
func (s *StreamHashBuilder) StoHint() byte { return s.b.StoHint() }

// LeafCount is the number of non-zero slots added.
func (s *StreamHashBuilder) LeafCount() int { return s.b.LeafCount() }
