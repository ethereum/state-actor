package ethrex

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ErrKeysOutOfOrder is returned by AddLeaf when the supplied key is not strictly
// greater than the previous key (nibble-lexicographic order).
var ErrKeysOutOfOrder = errors.New("Builder: keys must be inserted in strictly ascending nibble order")

// NodeSink is called by Builder once for each emitted trie row.
// pathNibbles is the raw (unprefixed) nibble path to the node; value is the
// node RLP (for branch/extension/leaf nodes) or the raw leaf value (for
// full-path leaf rows). See SPIKE_FINDINGS.md "Two rows per leaf".
//
// For storage tries, the caller prepends the 66-nibble address prefix to
// pathNibbles before writing to the DB. The Builder itself is keyspace-agnostic.
//
// A non-nil error from NodeSink is propagated back from AddLeaf or Root.
type NodeSink func(pathNibbles []byte, value []byte) error

// Builder is a streaming Merkle-Patricia trie builder that emits ethrex trie
// rows in the two-rows-per-leaf format required by account_trie_nodes and
// storage_trie_nodes column families.
//
// Leaves must be added in strictly ascending nibble-key order (ErrKeysOutOfOrder
// is returned otherwise). After all leaves have been added, call Root() to
// finalize the trie and obtain the root hash.
//
// # Emission contract
//
//   - Leaf nodes: two rows (node RLP row + full-path row).
//   - Branch/extension nodes: one row (node RLP).
//   - Empty trie (no leaves): one row ([] -> 0x80), root = EMPTY_TRIE_HASH.
//     This sentinel is correct ONLY for a standalone (state) trie. For a storage
//     trie with no slots, DO NOT call Root() through a storage-prefix sink — that
//     would write a bogus (prefix, 0x80) row. Instead the Phase 2 writer must
//     skip the Builder entirely for empty-storage accounts: emit zero storage
//     rows and set the account's storage_root = EMPTY_TRIE_HASH directly. See
//     SPIKE_FINDINGS.md ("Accounts with no storage emit ZERO storage rows").
//
// # Storage prefix
//
// pathNibbles emitted by the Builder are raw and unprefixed. For storage tries,
// wrap the NodeSink to prepend the 66-nibble storage prefix:
// nibbles(keccak256(addr)) ++ [LeafFlag] ++ [StorageSeparator] ++ path.
type Builder struct {
	sink      NodeSink
	prevKey   []byte
	keyLen    int // nibble length of all keys (set on first AddLeaf); -1 until set
	leaves    []leafEntry
	leafCount int
}

type leafEntry struct {
	key   []byte
	value []byte
}

// NewBuilder returns a Builder that emits node rows via sink.
// Pass nil for sink if only the root hash is needed (no rows will be emitted).
func NewBuilder(sink NodeSink) *Builder {
	if sink == nil {
		sink = func([]byte, []byte) error { return nil }
	}
	return &Builder{sink: sink}
}

// AddLeaf inserts a (keyNibbles, value) pair into the trie. keyNibbles must be
// strictly greater (nibble-lexicographic) than the previous call's key.
// value is the raw leaf value (e.g., RLP-encoded AccountState or storage value).
func (b *Builder) AddLeaf(keyNibbles []byte, value []byte) error {
	if b.prevKey != nil {
		if bytes.Compare(keyNibbles, b.prevKey) <= 0 {
			return ErrKeysOutOfOrder
		}
		// All keys must share the same nibble length. buildNode indexes
		// key[splitAt] unconditionally; mixed-length keys (where one key is a
		// prefix of another) would index out of bounds. In production every key
		// is keccak256(x) = 64 nibbles, so this only guards API misuse.
		if len(keyNibbles) != b.keyLen {
			return fmt.Errorf("Builder: all keys must have the same nibble length (got %d, want %d)", len(keyNibbles), b.keyLen)
		}
	} else {
		b.keyLen = len(keyNibbles)
	}
	b.leafCount++
	key := make([]byte, len(keyNibbles))
	copy(key, keyNibbles)
	val := make([]byte, len(value))
	copy(val, value)
	b.leaves = append(b.leaves, leafEntry{key: key, value: val})
	b.prevKey = key
	return nil
}

// Root finalizes the trie, emits all node rows via the sink, and returns the
// trie root hash.
//
// Empty trie (no leaves): emits ([], 0x80) and returns EMPTY_TRIE_HASH.
func (b *Builder) Root() (common.Hash, error) {
	if b.leafCount == 0 {
		if err := b.sink([]byte{}, []byte{0x80}); err != nil {
			return common.Hash{}, err
		}
		return common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"), nil
	}

	root := buildNode(b.leaves, 0)
	nodeRLP, err := emitNode(root, nil, b.sink)
	if err != nil {
		return common.Hash{}, err
	}
	return rlpToRootHash(nodeRLP), nil
}

// ---------------------------------------------------------------------------
// In-memory trie node types
// ---------------------------------------------------------------------------

type trieNode interface {
	isTrieNode()
}

// trieLeaf represents a leaf node. rem is the remaining path (suffix after the
// branch key nibble that led to this leaf's position); the row-2 full path is
// reconstructed in emitLeaf from path+rem. value is the raw leaf value.
type trieLeaf struct {
	rem   []byte // remaining nibbles relative to this leaf's parent position
	value []byte
}

type trieExtension struct {
	prefix []byte
	child  trieNode
}

type trieBranch struct {
	children [16]trieNode
	value    []byte // usually nil in pure-MPT account/storage tries
}

func (trieLeaf) isTrieNode()      {}
func (trieExtension) isTrieNode() {}
func (trieBranch) isTrieNode()    {}

// buildNode constructs a trie node from entries, all of which start diverging
// at nibble index `depth` (i.e., entries[i].key[0:depth] are all equal).
func buildNode(entries []leafEntry, depth int) trieNode {
	if len(entries) == 1 {
		e := entries[0]
		return trieLeaf{
			rem:   e.key[depth:],
			value: e.value,
		}
	}

	// Find the common prefix length across all entries starting from `depth`.
	maxCPL := len(entries[0].key) - depth
	for _, e := range entries[1:] {
		c := commonPrefixLen(entries[0].key[depth:], e.key[depth:])
		if c < maxCPL {
			maxCPL = c
		}
	}

	// Position where entries first diverge.
	splitAt := depth + maxCPL

	// Group entries by their nibble at position `splitAt`.
	var branch trieBranch
	start := 0
	for start < len(entries) {
		d := entries[start].key[splitAt]
		end := start + 1
		for end < len(entries) && entries[end].key[splitAt] == d {
			end++
		}
		branch.children[d] = buildNode(entries[start:end], splitAt+1)
		start = end
	}

	if maxCPL > 0 {
		return trieExtension{
			prefix: entries[0].key[depth : depth+maxCPL],
			child:  &branch,
		}
	}
	return &branch
}

// emitNode recursively encodes a trie node, emitting rows to sink.
// path is the nibble path from the trie root to this node's position.
// Returns the raw node RLP (used by the parent to compute its child reference).
func emitNode(node trieNode, path []byte, sink NodeSink) ([]byte, error) {
	switch n := node.(type) {
	case trieLeaf:
		return emitLeaf(n, path, sink)
	case *trieBranch:
		return emitBranch(n, path, sink)
	case trieExtension:
		return emitExtension(n, path, sink)
	default:
		panic("unknown trie node type")
	}
}

// emitLeaf emits two rows and returns the leaf node RLP.
func emitLeaf(n trieLeaf, path []byte, sink NodeSink) ([]byte, error) {
	nodeRLP := EncodeLeaf(n.rem, n.value)

	// Row 1: (path, node RLP)
	if err := sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}

	// Row 2: (path ++ rem ++ [LeafFlag], raw value)
	fullPath := make([]byte, len(path)+len(n.rem)+1)
	copy(fullPath, path)
	copy(fullPath[len(path):], n.rem)
	fullPath[len(fullPath)-1] = LeafFlag
	if err := sink(fullPath, n.value); err != nil {
		return nil, err
	}

	return nodeRLP, nil
}

// emitBranch recursively processes children, then encodes and emits the branch.
func emitBranch(n *trieBranch, path []byte, sink NodeSink) ([]byte, error) {
	var children [16][]byte
	for i, child := range n.children {
		if child == nil {
			continue
		}
		childPath := make([]byte, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = byte(i)

		childRLP, err := emitNode(child, childPath, sink)
		if err != nil {
			return nil, err
		}
		// Store the child's NodeRef (raw if < 32 bytes, keccak32 if >= 32 bytes).
		// EncodeBranch will call NodeRef again on these, so store the raw RLP.
		children[i] = childRLP
	}

	nodeRLP := EncodeBranch(children, n.value)

	if err := sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}

	return nodeRLP, nil
}

// emitExtension processes its child, then encodes and emits the extension node.
func emitExtension(n trieExtension, path []byte, sink NodeSink) ([]byte, error) {
	childPath := make([]byte, len(path)+len(n.prefix))
	copy(childPath, path)
	copy(childPath[len(path):], n.prefix)

	childRLP, err := emitNode(n.child, childPath, sink)
	if err != nil {
		return nil, err
	}

	// EncodeExtension takes the raw child RLP; it applies NodeRef internally.
	nodeRLP := EncodeExtension(n.prefix, childRLP)

	if err := sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}

	return nodeRLP, nil
}

// rlpToRootHash returns the trie root hash for a node's RLP.
// The root hash is always keccak256 of the RLP (even if < 32 bytes).
func rlpToRootHash(nodeRLP []byte) common.Hash {
	return crypto.Keccak256Hash(nodeRLP)
}

// cloneNibbles returns a copy of the nibble slice (never nil — returns empty slice).
func cloneNibbles(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
