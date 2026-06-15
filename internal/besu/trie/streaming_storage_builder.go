package trie

import (
	"bytes"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	gethrlp "github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/internal/besu"
)

// ErrSlotsOutOfOrder is returned by StreamingStorageBuilder.AddSlot when
// the new slotHash is less than or equal to the previous slotHash. The
// streaming algorithm assumes strictly ascending input.
var ErrSlotsOutOfOrder = errors.New("besu/trie: AddSlot called out of keccak-ascending order")

// StreamingStorageBuilder builds the per-account Bonsai storage trie in
// O(trie depth) memory by processing keccak-sorted slot inserts through a
// right-spine, mirroring the reth HashBuilder / geth StackTrie algorithm.
// Bonsai-specific:
//   - DB key is the raw-nibble location (1 byte/nibble), assembled
//     incrementally from current[0:lenFrom] (leaves/extensions) or
//     current[0:length] (branches).
//   - Roots whose RLP < 32 bytes are emitted explicitly at RootLocation
//     (matches StorageBuilder.Commit's small-root case).
//   - Nodes whose hash equals besu.EmptyTrieNodeHash are not emitted.
//
// Input contract: AddSlot calls MUST arrive in strictly ascending
// keccak256(slotKey) order; streamingtrie.IterateRoot provides this.
type StreamingStorageBuilder struct {
	addrHash common.Hash
	sink     NodeSink

	// prevPath is the keyPath(prevSlotHash) — 65 bytes, terminator 0x10
	// at index 64. nil until the first AddSlot.
	prevPath  []byte
	prevValue []byte

	// stack holds the right-spine of finalized child references in
	// ascending-depth order. Each entry is either inline RLP (len < 32)
	// or a 33-byte hash reference (0xa0 || keccak(rlp)).
	stack [][]byte
	// stateMasks[d] is the slot-occupancy bitset for the branch whose
	// location is prevPath[:d]. Bit i set iff slot i has a finalized
	// child somewhere on the stack.
	stateMasks []uint16

	leafCount int
}

// NewStreamingStorageBuilder constructs a streaming builder that emits
// trie nodes through the caller-supplied sink. Use this when parallel
// workers each need their own builder pointing at their own per-worker
// node sink — `Builder.BeginStreamingStorage` is fine for the single-
// goroutine path, but its returned builder writes through the parent
// Builder's shared sink and is unsafe under concurrent use.
//
// addrHash must be the keccak256(address) the storage trie belongs to;
// it's used as the prefix on every emitted trie-node write.
func NewStreamingStorageBuilder(sink NodeSink, addrHash common.Hash) *StreamingStorageBuilder {
	return &StreamingStorageBuilder{
		addrHash: addrHash,
		sink:     sink,
	}
}

// AddSlot inserts (slotHash → valueRLP) into the streaming Bonsai
// storage trie. slotHash MUST be strictly greater than the previous
// call's slotHash; out-of-order input returns ErrSlotsOutOfOrder.
func (sb *StreamingStorageBuilder) AddSlot(slotHash common.Hash, valueRLP []byte) error {
	newPath := keyPath(slotHash[:])
	if sb.prevPath != nil {
		if cmp := bytes.Compare(newPath, sb.prevPath); cmp <= 0 {
			return ErrSlotsOutOfOrder
		}
		if err := sb.update(newPath); err != nil {
			return err
		}
	}
	sb.leafCount++
	sb.prevPath = newPath
	// streamingtrie may reuse the valueRLP buffer between iterations —
	// copy so we can hold it until the next AddSlot or Commit.
	sb.prevValue = append([]byte(nil), valueRLP...)
	return nil
}

// AddLeaf is an alias for AddSlot — both append one (keccak-key, RLP value)
// leaf to the trie. The alias lets *StreamingStorageBuilder satisfy
// streamingtrie.HashBuilder without an adapter.
func (sb *StreamingStorageBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return sb.AddSlot(keyHash, valueRLP)
}

// Root finalises the streaming build and returns the storage trie root.
// Implements the streamingtrie.HashBuilder contract (alongside AddLeaf).
func (sb *StreamingStorageBuilder) Root() (common.Hash, error) {
	return sb.Commit()
}

// Commit finalises the streaming build and returns the storage trie root.
// Empty trie returns EmptyTrieNodeHash with zero sink calls. Small-root
// case (root RLP < 32 bytes) emits the root at RootLocation, matching
// the non-streaming StorageBuilder at builder.go:131-140.
func (sb *StreamingStorageBuilder) Commit() (common.Hash, error) {
	if sb.leafCount == 0 {
		return besu.EmptyTrieNodeHash, nil
	}
	if sb.prevPath != nil {
		if err := sb.update(nil); err != nil {
			return common.Hash{}, err
		}
		sb.prevPath = nil
		sb.prevValue = nil
	}
	if len(sb.stack) == 0 {
		// Defensive — leafCount > 0 implies stack is non-empty after
		// update(nil). Treat as empty if somehow drained.
		return besu.EmptyTrieNodeHash, nil
	}
	rootRef := sb.stack[len(sb.stack)-1]
	if len(rootRef) == 33 && rootRef[0] == 0xa0 {
		var h common.Hash
		copy(h[:], rootRef[1:])
		return h, nil
	}
	// Root RLP is inline (< 32 B). Emit at RootLocation explicitly.
	rootRLP := rootRef
	rootHash := NodeHash(rootRLP)
	if rootHash != besu.EmptyTrieNodeHash {
		if err := sb.sink.PutAccountStorageTrieNode(sb.addrHash, RootLocation, rootHash, rootRLP); err != nil {
			return common.Hash{}, err
		}
	}
	return rootHash, nil
}

// update processes the deferred previous leaf against the new succeeding
// key. If succeeding is nil, performs full finalisation (collapses the
// spine down to a single root reference).
//
// Mirrors reth's HashBuilder.update at internal/reth/hash_builder.go:223-303
// with three Bonsai adjustments: (1) location bytes built incrementally
// for every emit, (2) emit fires for leaves and extensions in addition
// to branches (reth only emits branch metadata for incremental
// re-execution), (3) HP-encoding via Bonsai's CompactEncode.
func (sb *StreamingStorageBuilder) update(succeeding []byte) error {
	buildExtensions := false
	current := sb.prevPath

	for {
		precedingExists := len(sb.stateMasks) > 0
		precedingLen := 0
		if precedingExists {
			precedingLen = len(sb.stateMasks) - 1
		}

		cpl := commonPrefixLen(succeeding, current)
		length := precedingLen
		if cpl > length {
			length = cpl
		}

		extraDigit := current[length]
		for len(sb.stateMasks) <= length {
			sb.stateMasks = append(sb.stateMasks, 0)
		}
		sb.stateMasks[length] |= uint16(1) << extraDigit

		lenFrom := length
		if len(succeeding) > 0 || precedingExists {
			lenFrom++
		}
		shortNodeKey := current[lenFrom:]

		if !buildExtensions {
			leafRLP := encodeLeafRLP(shortNodeKey, sb.prevValue)
			leafLocation := append([]byte(nil), current[:lenFrom]...)
			if err := sb.maybeEmit(leafLocation, leafRLP); err != nil {
				return err
			}
			sb.stack = append(sb.stack, refOfRLP(leafRLP))
		}

		if buildExtensions && len(shortNodeKey) > 0 {
			top := sb.stack[len(sb.stack)-1]
			sb.stack = sb.stack[:len(sb.stack)-1]
			extRLP := encodeExtensionRLP(shortNodeKey, top)
			extLocation := append([]byte(nil), current[:lenFrom]...)
			if err := sb.maybeEmit(extLocation, extRLP); err != nil {
				return err
			}
			sb.stack = append(sb.stack, refOfRLP(extRLP))
		}

		if precedingLen <= cpl && len(succeeding) > 0 {
			return nil
		}

		if len(succeeding) > 0 || precedingExists {
			if err := sb.sealBranchAt(current, length); err != nil {
				return err
			}
		}

		sb.stateMasks = sb.stateMasks[:length]
		for len(sb.stateMasks) > 0 && sb.stateMasks[len(sb.stateMasks)-1] == 0 {
			sb.stateMasks = sb.stateMasks[:len(sb.stateMasks)-1]
		}

		if precedingLen == 0 {
			return nil
		}
		current = current[:precedingLen]
		buildExtensions = true
	}
}

// sealBranchAt pops popcount(stateMasks[length]) children from the stack,
// encodes them as a 17-RLP-item branch node, emits at location
// current[:length], and pushes the branch's ref back onto the stack.
func (sb *StreamingStorageBuilder) sealBranchAt(current []byte, length int) error {
	stateMask := sb.stateMasks[length]
	childCount := popcount16(stateMask)
	firstChild := len(sb.stack) - childCount
	children := sb.stack[firstChild:]

	branchRLP := encodeBranchRLP(stateMask, children)
	branchLocation := append([]byte(nil), current[:length]...)
	if err := sb.maybeEmit(branchLocation, branchRLP); err != nil {
		return err
	}

	sb.stack = sb.stack[:firstChild]
	sb.stack = append(sb.stack, refOfRLP(branchRLP))
	return nil
}

// maybeEmit fires PutAccountStorageTrieNode for non-inline nodes whose
// hash is not EmptyTrieNodeHash. Mirrors builder.go:323-333 (maybeEmit).
func (sb *StreamingStorageBuilder) maybeEmit(location, rlp []byte) error {
	if !IsReferencedByHash(rlp) {
		return nil
	}
	hash := NodeHash(rlp)
	if hash == besu.EmptyTrieNodeHash {
		return nil
	}
	return sb.sink.PutAccountStorageTrieNode(sb.addrHash, location, hash, rlp)
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
// [CompactEncode(path, false), childRef]. childRef is the byte sequence
// a parent uses to reference the child (inline RLP for small children,
// 0xa0 || keccak for hashed). Matches extensionNode.EncodedBytes at
// node.go:119-136.
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

// encodeBranchRLP encodes a 17-item branch RLP. stateMask indicates which
// slots are occupied (bit i = slot i has a child); occupied slots are
// taken from children in ascending order. Empty slots and the 17th
// value slot encode as 0x80 (RLP null). Matches branchNode.EncodedBytes
// at node.go:177-197.
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

// refOfRLP returns the byte sequence a parent uses to reference a child
// node, matching EncodedBytesRef at hash.go:31-43.
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
