package trie

import (
	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/internal/besu"
)

// NodeSink receives trie-node writes from a Builder. The Bonsai client
// adapter (client/besu/) implements this interface to write to its
// TRIE_BRANCH_STORAGE column family.
//
// Citations: BonsaiWorldStateKeyValueStorage.java:284-311 (Builder side)
// and BonsaiWorldStateKeyValueStorage.java:273-282 (SaveWorldState).
type NodeSink interface {
	// PutAccountStateTrieNode writes one node of the account state trie at
	// the given location (one byte per nibble path-from-root).
	//
	// Implementations may skip the write when hash == besu.EmptyTrieNodeHash
	// per BonsaiWorldStateKeyValueStorage.java:286-288.
	PutAccountStateTrieNode(location []byte, hash common.Hash, nodeRLP []byte) error

	// PutAccountStorageTrieNode writes one node of a per-account storage
	// trie. The DB key is accountHash(32) ++ location (variable length).
	PutAccountStorageTrieNode(accountHash common.Hash, location []byte, hash common.Hash, nodeRLP []byte) error

	// SaveWorldState is called once at the end of generation to write the
	// three world-state sentinels:
	//   - TRIE_BRANCH_STORAGE[Bytes.EMPTY]    = rootRLP
	//   - TRIE_BRANCH_STORAGE["worldRoot"]    = rootHash
	//   - TRIE_BRANCH_STORAGE["worldBlockHash"] = blockHash
	//
	// Note: Builder does NOT invoke SaveWorldState — the caller invokes
	// it after computing the genesis block hash from the rootHash
	// returned by Commit.
	SaveWorldState(blockHash common.Hash, rootHash common.Hash, rootRLP []byte) error
}

// Builder is the in-memory (non-streaming) Bonsai account-state trie builder.
// It maintains the entire account MPT resident (functional inserts) and emits
// non-inline nodes via the NodeSink only at Commit time (post-order traversal),
// so its memory is O(accounts). Production bulk generation uses
// StreamingAccountBuilder (O(trie depth), in streaming_account_builder.go);
// Builder is retained as the byte-identical reference oracle in tests.
//
// It is insert-only and assumes inputs arrive in keccak-sorted addrHash order.
type Builder struct {
	sink  NodeSink
	root  Node
	count int
}

// New returns a Builder that will emit non-inline trie nodes through sink.
func New(sink NodeSink) *Builder {
	return &Builder{sink: sink, root: NullNodeInstance}
}

// AddAccount inserts (addrHash → accountRLP) into the account-state trie.
// addrHash must be a keccak256(address). accountRLP is the leaf value (the
// flat account RLP from internal/besu/rlp.EncodeAccount).
//
// Inputs SHOULD be supplied in ascending addrHash order for performance,
// but the trie root is correct regardless of insertion order.
func (b *Builder) AddAccount(addrHash common.Hash, accountRLP []byte) error {
	path := keyPath(addrHash[:])
	b.root = insertNode(b.root, path, accountRLP)
	b.count++
	return nil
}

// BeginStorage returns a StorageBuilder for the per-account storage trie.
// The returned builder shares the parent Builder's sink so storage-trie
// nodes are emitted under the addrHash-prefixed key space.
func (b *Builder) BeginStorage(addrHash common.Hash) *StorageBuilder {
	return &StorageBuilder{
		addrHash: addrHash,
		sink:     b.sink,
		root:     NullNodeInstance,
	}
}

// BeginStreamingStorage returns a StreamingStorageBuilder bound to this
// Builder's sink. Retained for the in-memory Builder API and tests — production
// code constructs NewStreamingStorageBuilder directly. Builds in O(trie depth)
// memory by consuming AddSlot calls in keccak-ascending order via a right-spine
// algorithm.
func (b *Builder) BeginStreamingStorage(addrHash common.Hash) *StreamingStorageBuilder {
	return NewStreamingStorageBuilder(b.sink, addrHash)
}

// Commit finalizes the account-state trie and emits all non-inline nodes via
// the sink. Returns the root hash and root RLP. Caller is responsible for
// calling sink.SaveWorldState once the genesis block hash is computed.
//
// For an empty trie (no AddAccount calls): returns EmptyTrieNodeHash, [0x80],
// nil. No sink calls fire.
func (b *Builder) Commit() (common.Hash, []byte, error) {
	if b.count == 0 {
		return besu.EmptyTrieNodeHash, []byte{0x80}, nil
	}
	if err := commitTraversal(b.sink, RootLocation, b.root, accountTrieEmitter{}); err != nil {
		return common.Hash{}, nil, err
	}
	rootRLP := b.root.EncodedBytes()
	rootHash := b.root.Hash()
	// Small-root special case: when the root's RLP is < 32 bytes the
	// post-order traversal does NOT emit it (its parent — none in this
	// case — would have inlined it). Mirror StoredMerkleTrie.java:151-153
	// which always stores the root at the empty location regardless of size.
	if !IsReferencedByHash(rootRLP) {
		if err := b.sink.PutAccountStateTrieNode(RootLocation, rootHash, rootRLP); err != nil {
			return common.Hash{}, nil, err
		}
	}
	return rootHash, rootRLP, nil
}

// StorageBuilder is the per-account storage-trie counterpart to Builder.
type StorageBuilder struct {
	addrHash common.Hash
	sink     NodeSink
	root     Node
	count    int
}

// AddSlot inserts (slotHash → valueRLP) into the storage trie.
func (sb *StorageBuilder) AddSlot(slotHash common.Hash, valueRLP []byte) error {
	path := keyPath(slotHash[:])
	sb.root = insertNode(sb.root, path, valueRLP)
	sb.count++
	return nil
}

// Commit finalizes the storage trie. Returns the storage root hash. Empty
// trie returns EmptyTrieNodeHash with no sink calls.
func (sb *StorageBuilder) Commit() (common.Hash, error) {
	if sb.count == 0 {
		return besu.EmptyTrieNodeHash, nil
	}
	if err := commitTraversal(sb.sink, RootLocation, sb.root, storageTrieEmitter{addrHash: sb.addrHash}); err != nil {
		return common.Hash{}, err
	}
	rootRLP := sb.root.EncodedBytes()
	rootHash := sb.root.Hash()
	if !IsReferencedByHash(rootRLP) {
		// Small storage root: write at addrHash-prefixed empty location.
		if err := sb.sink.PutAccountStorageTrieNode(sb.addrHash, RootLocation, rootHash, rootRLP); err != nil {
			return common.Hash{}, err
		}
	}
	return rootHash, nil
}

// keyPath converts a 32-byte hash to a 65-nibble path (64 nibbles + leaf
// terminator at position 64). Each nibble occupies one byte.
//
// Mirrors CompactEncoding.bytesToPath at hyperledger/besu tag 26.5.0.
func keyPath(key []byte) []byte {
	out := make([]byte, len(key)*2+1)
	for i, b := range key {
		out[2*i] = b >> 4
		out[2*i+1] = b & 0x0f
	}
	out[len(key)*2] = besu.LeafTerminator
	return out
}

// insertNode is the recursive Bonsai trie insert. It returns a (possibly new)
// node — the tree is functional (no in-place mutation of existing nodes).
//
// path is the *remaining* nibble suffix at the current depth, including the
// trailing 0x10 terminator. value is the raw bytes to store at the leaf.
func insertNode(node Node, path []byte, value []byte) Node {
	switch n := node.(type) {
	case nullNode:
		return NewLeafNode(path, value)

	case *leafNode:
		// Path of an existing leaf MUST equal the new path for it to be
		// the same key. Otherwise we split.
		if equalNibbles(n.path, path) {
			// Replace value (we use unique addrHashes so this is rare).
			return NewLeafNode(path, value)
		}
		// Find longest common prefix of n.path and path.
		cpl := commonPrefixLen(n.path, path)
		br := NewBranchNode()
		// Place the existing leaf with its path suffix beyond the cpl.
		oldSuffix := n.path[cpl:]
		newSuffix := path[cpl:]
		br.SetChild(int(oldSuffix[0]), NewLeafNode(oldSuffix[1:], n.value))
		br.SetChild(int(newSuffix[0]), NewLeafNode(newSuffix[1:], value))
		if cpl == 0 {
			return br
		}
		return NewExtensionNode(append([]byte(nil), n.path[:cpl]...), br)

	case *extensionNode:
		cpl := commonPrefixLen(n.path, path)
		if cpl == len(n.path) {
			// Full extension consumed; descend into child.
			newChild := insertNode(n.child, path[cpl:], value)
			return NewExtensionNode(append([]byte(nil), n.path...), newChild)
		}
		// Partial overlap: split the extension.
		br := NewBranchNode()
		// Existing extension's tail beyond cpl.
		oldRest := n.path[cpl:]
		// If the existing extension has only one remaining nibble, that
		// child slot becomes its child directly (no degenerate 0-length
		// extension); otherwise wrap in a shorter extension.
		var oldChild Node
		if len(oldRest) == 1 {
			oldChild = n.child
		} else {
			oldChild = NewExtensionNode(append([]byte(nil), oldRest[1:]...), n.child)
		}
		br.SetChild(int(oldRest[0]), oldChild)
		// New leaf for the inserted path.
		newSuffix := path[cpl:]
		br.SetChild(int(newSuffix[0]), NewLeafNode(newSuffix[1:], value))
		if cpl == 0 {
			return br
		}
		return NewExtensionNode(append([]byte(nil), n.path[:cpl]...), br)

	case *branchNode:
		// Should always have at least one nibble left to consume because
		// each leaf path has 65 elements (64 nibbles + terminator) and
		// branches consume one nibble per descent. A path that runs out at
		// a branch would imply a value-bearing branch (slot 17) — not used
		// by our state/storage tries.
		nib := path[0]
		// Make a copy with the updated child slot.
		newBr := NewBranchNode()
		for i := 0; i < 16; i++ {
			newBr.SetChild(i, n.children[i])
		}
		newBr.SetChild(int(nib), insertNode(n.children[nib], path[1:], value))
		newBr.value = n.value
		return newBr

	default:
		panic("besu/trie: insertNode: unreachable node type")
	}
}

func equalNibbles(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// emitter abstracts the difference between account-trie writes and
// storage-trie writes (which carry an addrHash prefix in the DB key).
type emitter interface {
	emit(sink NodeSink, location []byte, hash common.Hash, rlp []byte) error
}

type accountTrieEmitter struct{}

func (accountTrieEmitter) emit(sink NodeSink, location []byte, hash common.Hash, rlp []byte) error {
	return sink.PutAccountStateTrieNode(location, hash, rlp)
}

type storageTrieEmitter struct {
	addrHash common.Hash
}

func (e storageTrieEmitter) emit(sink NodeSink, location []byte, hash common.Hash, rlp []byte) error {
	return sink.PutAccountStorageTrieNode(e.addrHash, location, hash, rlp)
}

// commitTraversal performs post-order trie traversal, emitting non-inline
// nodes via the emitter. The location parameter tracks the nibble path from
// root using one byte per nibble (Bonsai DB-key convention).
//
// Empty-tree short-circuit: nodes with hash == EmptyTrieNodeHash are NOT
// emitted (matches BonsaiWorldStateKeyValueStorage.java:286-288).
func commitTraversal(sink NodeSink, location []byte, node Node, em emitter) error {
	switch n := node.(type) {
	case nullNode:
		return nil

	case *extensionNode:
		// Descend through the extension's nibble path before emitting self.
		if err := commitTraversal(sink, AppendPath(location, n.path), n.child, em); err != nil {
			return err
		}
		return maybeEmit(em, sink, location, n)

	case *branchNode:
		for i := 0; i < 16; i++ {
			if _, isNull := n.children[i].(nullNode); isNull {
				continue
			}
			childLoc := AppendNibble(location, byte(i))
			if err := commitTraversal(sink, childLoc, n.children[i], em); err != nil {
				return err
			}
		}
		return maybeEmit(em, sink, location, n)

	case *leafNode:
		return maybeEmit(em, sink, location, n)

	default:
		panic("besu/trie: commitTraversal: unreachable")
	}
}

// maybeEmit calls the emitter only if the node's RLP is >= 32 bytes (i.e.,
// the parent would store it by reference rather than inline).
func maybeEmit(em emitter, sink NodeSink, location []byte, n Node) error {
	rlp := n.EncodedBytes()
	if !IsReferencedByHash(rlp) {
		return nil
	}
	hash := n.Hash()
	if hash == besu.EmptyTrieNodeHash {
		return nil
	}
	return em.emit(sink, location, hash, rlp)
}
