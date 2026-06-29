package trie

import (
	"bytes"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	gethrlp "github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/internal/besu"
)

// ErrSlotsOutOfOrder is returned when a streaming builder receives a key that
// is less than or equal to the previous key. The streaming algorithm assumes
// strictly ascending keccak-key input (slots for storage, addrHashes for the
// account trie).
var ErrSlotsOutOfOrder = errors.New("besu/trie: streaming leaf added out of keccak-ascending order")

// streamingBuilder is the shared right-spine engine for both the account-state
// trie and the per-account storage tries. It builds a Bonsai MPT in O(trie
// depth) memory by processing keccak-sorted leaf inserts through a right-spine,
// mirroring the reth HashBuilder / geth StackTrie algorithm. The only
// account-vs-storage difference is the node-emit target, supplied via em:
//   - accountTrieEmitter → PutAccountStateTrieNode(location, …)
//   - storageTrieEmitter → PutAccountStorageTrieNode(addrHash, location, …)
//
// Bonsai-specific:
//   - DB key is the raw-nibble location (1 byte/nibble), assembled
//     incrementally from current[0:lenFrom] (leaves/extensions) or
//     current[0:length] (branches).
//   - Roots whose RLP < 32 bytes are emitted explicitly at RootLocation
//     (matches the non-streaming builders' small-root case).
//   - Nodes whose hash equals besu.EmptyTrieNodeHash are not emitted.
type streamingBuilder struct {
	sink NodeSink
	em   emitter

	// prevPath is the keyPath(prevKeyHash) — 65 bytes, terminator 0x10 at
	// index 64. nil until the first leaf.
	prevPath  []byte
	prevValue []byte

	// stack holds the right-spine of finalized child references in
	// ascending-depth order. Each entry is either inline RLP (len < 32) or a
	// 33-byte hash reference (0xa0 || keccak(rlp)).
	stack [][]byte
	// stateMasks[d] is the slot-occupancy bitset for the branch whose location
	// is prevPath[:d]. Bit i set iff slot i has a finalized child on the stack.
	stateMasks []uint16

	leafCount int

	// rootRLP captures the full RLP of the root node (the one emitted at the
	// empty RootLocation). Needed by the account caller for SaveWorldState; the
	// storage caller ignores it.
	rootRLP []byte
}

// addLeaf inserts (keyHash → valueRLP). keyHash MUST be strictly greater than
// the previous call's keyHash; out-of-order input returns ErrSlotsOutOfOrder.
func (c *streamingBuilder) addLeaf(keyHash common.Hash, valueRLP []byte) error {
	newPath := keyPath(keyHash[:])
	if c.prevPath != nil {
		if cmp := bytes.Compare(newPath, c.prevPath); cmp <= 0 {
			return ErrSlotsOutOfOrder
		}
		if err := c.update(newPath); err != nil {
			return err
		}
	}
	c.leafCount++
	c.prevPath = newPath
	// The caller may reuse the valueRLP buffer between iterations — copy so we
	// can hold it until the next addLeaf or finalize.
	c.prevValue = append([]byte(nil), valueRLP...)
	return nil
}

// finalize collapses the spine and returns (rootHash, rootRLP). Empty trie
// returns EmptyTrieNodeHash with the canonical empty-RLP [0x80] and zero sink
// calls. Small-root case (root RLP < 32 bytes) emits the root at RootLocation,
// matching the non-streaming builders.
func (c *streamingBuilder) finalize() (common.Hash, []byte, error) {
	if c.leafCount == 0 {
		return besu.EmptyTrieNodeHash, []byte{0x80}, nil
	}
	if c.prevPath != nil {
		if err := c.update(nil); err != nil {
			return common.Hash{}, nil, err
		}
		c.prevPath = nil
		c.prevValue = nil
	}
	if len(c.stack) == 0 {
		// Defensive — leafCount > 0 implies stack is non-empty after update(nil).
		return besu.EmptyTrieNodeHash, []byte{0x80}, nil
	}
	rootRef := c.stack[len(c.stack)-1]
	if len(rootRef) == 33 && rootRef[0] == 0xa0 {
		var h common.Hash
		copy(h[:], rootRef[1:])
		// Hashed root: its full RLP was captured by maybeEmit at RootLocation.
		return h, c.rootRLP, nil
	}
	// Root RLP is inline (< 32 B). Emit at RootLocation explicitly.
	rootRLP := rootRef
	rootHash := NodeHash(rootRLP)
	if rootHash != besu.EmptyTrieNodeHash {
		if err := c.em.emit(c.sink, RootLocation, rootHash, rootRLP); err != nil {
			return common.Hash{}, nil, err
		}
	}
	return rootHash, rootRLP, nil
}

// update processes the deferred previous leaf against the new succeeding key.
// If succeeding is nil, performs full finalisation (collapses the spine down to
// a single root reference).
//
// Mirrors reth's HashBuilder.update at internal/reth/hash_builder.go:223-303
// with three Bonsai adjustments: (1) location bytes built incrementally for
// every emit, (2) emit fires for leaves and extensions in addition to branches
// (reth only emits branch metadata for incremental re-execution), (3) HP-encoding
// via Bonsai's CompactEncode.
func (c *streamingBuilder) update(succeeding []byte) error {
	buildExtensions := false
	current := c.prevPath

	for {
		precedingExists := len(c.stateMasks) > 0
		precedingLen := 0
		if precedingExists {
			precedingLen = len(c.stateMasks) - 1
		}

		cpl := commonPrefixLen(succeeding, current)
		length := precedingLen
		if cpl > length {
			length = cpl
		}

		extraDigit := current[length]
		for len(c.stateMasks) <= length {
			c.stateMasks = append(c.stateMasks, 0)
		}
		c.stateMasks[length] |= uint16(1) << extraDigit

		lenFrom := length
		if len(succeeding) > 0 || precedingExists {
			lenFrom++
		}
		shortNodeKey := current[lenFrom:]

		if !buildExtensions {
			leafRLP := encodeLeafRLP(shortNodeKey, c.prevValue)
			leafLocation := append([]byte(nil), current[:lenFrom]...)
			if err := c.maybeEmit(leafLocation, leafRLP); err != nil {
				return err
			}
			c.stack = append(c.stack, refOfRLP(leafRLP))
		}

		if buildExtensions && len(shortNodeKey) > 0 {
			top := c.stack[len(c.stack)-1]
			c.stack = c.stack[:len(c.stack)-1]
			extRLP := encodeExtensionRLP(shortNodeKey, top)
			extLocation := append([]byte(nil), current[:lenFrom]...)
			if err := c.maybeEmit(extLocation, extRLP); err != nil {
				return err
			}
			c.stack = append(c.stack, refOfRLP(extRLP))
		}

		if precedingLen <= cpl && len(succeeding) > 0 {
			return nil
		}

		if len(succeeding) > 0 || precedingExists {
			if err := c.sealBranchAt(current, length); err != nil {
				return err
			}
		}

		c.stateMasks = c.stateMasks[:length]
		for len(c.stateMasks) > 0 && c.stateMasks[len(c.stateMasks)-1] == 0 {
			c.stateMasks = c.stateMasks[:len(c.stateMasks)-1]
		}

		if precedingLen == 0 {
			return nil
		}
		current = current[:precedingLen]
		buildExtensions = true
	}
}

// sealBranchAt pops popcount(stateMasks[length]) children from the stack,
// encodes them as a 17-RLP-item branch node, emits at location current[:length],
// and pushes the branch's ref back onto the stack.
func (c *streamingBuilder) sealBranchAt(current []byte, length int) error {
	stateMask := c.stateMasks[length]
	childCount := popcount16(stateMask)
	firstChild := len(c.stack) - childCount
	children := c.stack[firstChild:]

	branchRLP := encodeBranchRLP(stateMask, children)
	branchLocation := append([]byte(nil), current[:length]...)
	if err := c.maybeEmit(branchLocation, branchRLP); err != nil {
		return err
	}

	c.stack = c.stack[:firstChild]
	c.stack = append(c.stack, refOfRLP(branchRLP))
	return nil
}

// maybeEmit fires em.emit for non-inline nodes whose hash is not
// EmptyTrieNodeHash. Mirrors builder.go's maybeEmit. The root node — the only
// one emitted at the empty RootLocation — has its full RLP captured so the
// caller can recover it after finalize.
func (c *streamingBuilder) maybeEmit(location, rlp []byte) error {
	if !IsReferencedByHash(rlp) {
		return nil
	}
	hash := NodeHash(rlp)
	if hash == besu.EmptyTrieNodeHash {
		return nil
	}
	if len(location) == 0 {
		c.rootRLP = append([]byte(nil), rlp...)
	}
	return c.em.emit(c.sink, location, hash, rlp)
}

// StreamingStorageBuilder builds the per-account Bonsai storage trie in O(trie
// depth) memory. It is a thin wrapper over the shared streamingBuilder engine
// with a storage emitter (addrHash-prefixed node keys).
//
// Input contract: AddSlot/AddLeaf calls MUST arrive in strictly ascending
// keccak256(slotKey) order; streamingtrie.IterateRoot provides this.
type StreamingStorageBuilder struct {
	c streamingBuilder
}

// NewStreamingStorageBuilder constructs a streaming storage builder that emits
// trie nodes through the caller-supplied sink. Use this when parallel workers
// each need their own builder pointing at their own per-worker node sink —
// `Builder.BeginStreamingStorage` is fine for the single-goroutine path, but its
// returned builder writes through the parent Builder's shared sink and is unsafe
// under concurrent use.
//
// addrHash must be the keccak256(address) the storage trie belongs to; it's used
// as the prefix on every emitted trie-node write.
func NewStreamingStorageBuilder(sink NodeSink, addrHash common.Hash) *StreamingStorageBuilder {
	return &StreamingStorageBuilder{c: streamingBuilder{sink: sink, em: storageTrieEmitter{addrHash: addrHash}}}
}

// AddSlot inserts (slotHash → valueRLP) into the streaming storage trie.
// slotHash MUST be strictly greater than the previous call's; out-of-order
// input returns ErrSlotsOutOfOrder.
func (sb *StreamingStorageBuilder) AddSlot(slotHash common.Hash, valueRLP []byte) error {
	return sb.c.addLeaf(slotHash, valueRLP)
}

// AddLeaf is an alias for AddSlot — both append one (keccak-key, RLP value)
// leaf. The alias lets *StreamingStorageBuilder satisfy streamingtrie.HashBuilder.
func (sb *StreamingStorageBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return sb.c.addLeaf(keyHash, valueRLP)
}

// Commit finalises the build and returns the storage trie root. Empty trie
// returns EmptyTrieNodeHash with zero sink calls.
func (sb *StreamingStorageBuilder) Commit() (common.Hash, error) {
	h, _, err := sb.c.finalize()
	return h, err
}

// Root finalises the build and returns the storage trie root. Implements the
// streamingtrie.HashBuilder contract (alongside AddLeaf).
func (sb *StreamingStorageBuilder) Root() (common.Hash, error) {
	return sb.Commit()
}

// encodeLeafRLP encodes a leaf node as RLP [CompactEncode(path, true), value].
// Strips the trailing 0x10 terminator from path before HP-encoding, matching
// leafNode.EncodedBytes at node.go:79-82.
func encodeLeafRLP(path, value []byte) []byte {
	pathNibbles := path
	if n := len(pathNibbles); n > 0 && pathNibbles[n-1] == besu.LeafTerminator {
		pathNibbles = pathNibbles[:n-1]
	}
	hp := CompactEncode(pathNibbles, true)
	rlp, err := gethrlp.EncodeToBytes([][]byte{hp, value})
	if err != nil {
		panic("besu/trie: leaf RLP encode: " + err.Error())
	}
	return rlp
}

// encodeExtensionRLP encodes an extension node as RLP
// [CompactEncode(path, false), childRef]. childRef is the byte sequence a parent
// uses to reference the child (inline RLP for small children, 0xa0 || keccak for
// hashed). Matches extensionNode.EncodedBytes at node.go:119-136.
func encodeExtensionRLP(path, childRef []byte) []byte {
	hp := CompactEncode(path, false)
	rlp, err := gethrlp.EncodeToBytes([]gethrlp.RawValue{
		mustEncodeBytes(hp),
		gethrlp.RawValue(childRef),
	})
	if err != nil {
		panic("besu/trie: extension RLP encode: " + err.Error())
	}
	return rlp
}

// encodeBranchRLP encodes a 17-item branch RLP. stateMask indicates which slots
// are occupied (bit i = slot i has a child); occupied slots are taken from
// children in ascending order. Empty slots and the 17th value slot encode as
// 0x80 (RLP null). Matches branchNode.EncodedBytes at node.go:177-197.
func encodeBranchRLP(stateMask uint16, children [][]byte) []byte {
	items := make([]gethrlp.RawValue, 17)
	childIdx := 0
	for slot := uint16(0); slot < 16; slot++ {
		if stateMask&(uint16(1)<<slot) != 0 {
			items[slot] = gethrlp.RawValue(children[childIdx])
			childIdx++
		} else {
			items[slot] = gethrlp.RawValue([]byte{0x80})
		}
	}
	items[16] = gethrlp.RawValue([]byte{0x80})
	rlp, err := gethrlp.EncodeToBytes(items)
	if err != nil {
		panic("besu/trie: branch RLP encode: " + err.Error())
	}
	return rlp
}

// refOfRLP returns the byte sequence a parent uses to reference a child node,
// matching EncodedBytesRef at hash.go:31-43.
func refOfRLP(rlp []byte) []byte {
	if !IsReferencedByHash(rlp) {
		out := make([]byte, len(rlp))
		copy(out, rlp)
		return out
	}
	hash := NodeHash(rlp)
	out := make([]byte, 33)
	out[0] = 0xa0
	copy(out[1:], hash[:])
	return out
}

func popcount16(x uint16) int {
	n := 0
	for x != 0 {
		n += int(x & 1)
		x >>= 1
	}
	return n
}
