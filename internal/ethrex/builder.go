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
// full-path leaf rows). See the "two rows per leaf" model below.
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
// # Memory
//
// The builder is fully streaming: it never buffers the leaf set. It keeps only
// the rightmost spine of open branch nodes on a stack (at most keyLen frames,
// 16 child refs each) plus the single most-recently-added leaf. Peak memory is
// O(keyLen) — independent of the number of leaves — so it scales to multi-GB
// state without an O(n) RAM ceiling. Node rows are handed to the sink the moment
// a subtree is finalized.
//
// The emitted row set is identical to a recursive build-then-emit pass: the trie
// structure is a pure function of the (sorted) keys, and RocksDB stores by key,
// so emission order is irrelevant. builder_reference_test.go pins this with a
// randomized differential test against the recursive reference implementation.
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
//     rows and set the account's storage_root = EMPTY_TRIE_HASH directly
//     (accounts with no storage emit ZERO storage rows).
//
// # Storage prefix
//
// pathNibbles emitted by the Builder are raw and unprefixed. For storage tries,
// wrap the NodeSink to prepend the 66-nibble storage prefix:
// nibbles(keccak256(addr)) ++ [LeafFlag] ++ [StorageSeparator] ++ path.
type Builder struct {
	sink      NodeSink
	started   bool
	keyLen    int           // nibble length of all keys (set on first AddLeaf)
	prevKey   []byte        // most recent key (owned copy); the open spine leaf
	prevVal   []byte        // most recent value (owned copy)
	stack     []*openBranch // root-to-deepest spine of branches still gaining children
	leafCount int
}

// openBranch is a branch node on the rightmost spine that may still receive more
// children. depth is the nibble index it splits on. children holds the raw RLP
// of already-finalized children (those to the left of the still-open child); the
// open child is implicit (it is prevKey's descendant) and is committed when the
// next divergence closes it.
type openBranch struct {
	depth    int
	children [16][]byte
}

// NewBuilder returns a Builder that emits node rows via sink.
// Pass nil for sink if only the root hash is needed (no rows will be emitted).
func NewBuilder(sink NodeSink) *Builder {
	if sink == nil {
		sink = func([]byte, []byte) error { return nil }
	}
	return &Builder{sink: sink, keyLen: -1}
}

// AddLeaf inserts a (keyNibbles, value) pair into the trie. keyNibbles must be
// strictly greater (nibble-lexicographic) than the previous call's key.
// value is the raw leaf value (e.g., RLP-encoded AccountState or storage value).
func (b *Builder) AddLeaf(keyNibbles []byte, value []byte) error {
	if !b.started {
		b.started = true
		b.keyLen = len(keyNibbles)
		b.prevKey = append([]byte(nil), keyNibbles...)
		b.prevVal = append([]byte(nil), value...)
		b.leafCount = 1
		return nil
	}

	if bytes.Compare(keyNibbles, b.prevKey) <= 0 {
		return ErrKeysOutOfOrder
	}
	// All keys must share the same nibble length. The spine logic indexes
	// key[depth] unconditionally; mixed-length keys (where one key is a prefix
	// of another) would index out of bounds. In production every key is
	// keccak256(x) = 64 nibbles, so this only guards API misuse.
	if len(keyNibbles) != b.keyLen {
		return fmt.Errorf("Builder: all keys must have the same nibble length (got %d, want %d)", len(keyNibbles), b.keyLen)
	}

	if err := b.insert(keyNibbles); err != nil {
		return err
	}
	b.leafCount++
	b.prevKey = append(b.prevKey[:0:0], keyNibbles...)
	b.prevVal = append(b.prevVal[:0:0], value...)
	return nil
}

// insert folds the previous (now-closed) leaf into the spine and positions the
// spine to receive the new key as its open leaf. The new key/value are committed
// into b.prevKey/b.prevVal by the caller after this returns.
func (b *Builder) insert(key []byte) error {
	lcp := commonPrefixLen(b.prevKey, key)
	dTop := -1
	if len(b.stack) > 0 {
		dTop = b.stack[len(b.stack)-1].depth
	}
	d := lcp
	if dTop > d {
		d = dTop
	}

	// Finalize the previous leaf as a child at depth d.
	prevLeafRLP, err := b.emitLeafRows(b.prevKey[:d+1], b.prevKey[d+1:], b.prevVal)
	if err != nil {
		return err
	}

	if d > dTop {
		// lcp > dTop (or empty stack): the previous and new leaf share more than
		// the deepest branch covers, so they form a new branch at depth d. The
		// previous leaf is its first child; the new key will be its open child.
		nb := &openBranch{depth: d}
		nb.children[b.prevKey[d]] = prevLeafRLP
		b.stack = append(b.stack, nb)
		return nil
	}

	// d == dTop, so lcp <= dTop: the previous leaf is a child of the top branch.
	top := b.stack[len(b.stack)-1]
	top.children[b.prevKey[dTop]] = prevLeafRLP
	if lcp == dTop {
		// New key is another child of the same branch; nothing to fold.
		return nil
	}
	// lcp < dTop: the top branch (and any deeper than lcp) is complete. Fold it
	// up and attach the new key at depth lcp (an existing branch there, or a new
	// split branch).
	return b.foldTo(lcp, key)
}

// foldTo pops every spine branch deeper than lcp, finalizing each and attaching
// it to its parent. The new key (diverging at lcp) is then positioned: either as
// a new child of an existing branch at depth lcp, or — if there is no branch at
// lcp (a straight extension run is split here) — under a freshly inserted branch
// at depth lcp.
func (b *Builder) foldTo(lcp int, key []byte) error {
	for {
		p := b.stack[len(b.stack)-1]
		b.stack = b.stack[:len(b.stack)-1]

		parentIsDeep := len(b.stack) > 0 && b.stack[len(b.stack)-1].depth > lcp
		parentDepth := lcp
		if parentIsDeep {
			parentDepth = b.stack[len(b.stack)-1].depth
		}
		pref, err := b.finalizeBranch(p, parentDepth)
		if err != nil {
			return err
		}

		if parentIsDeep {
			// Parent is also being folded; attach normally and keep going.
			parent := b.stack[len(b.stack)-1]
			parent.children[b.prevKey[parent.depth]] = pref
			continue
		}

		// p is the shallowest branch deeper than lcp; its effective parent is the
		// branch at depth lcp (existing or new).
		if len(b.stack) > 0 && b.stack[len(b.stack)-1].depth == lcp {
			top := b.stack[len(b.stack)-1]
			top.children[b.prevKey[lcp]] = pref
		} else {
			nb := &openBranch{depth: lcp}
			nb.children[b.prevKey[lcp]] = pref
			b.stack = append(b.stack, nb)
		}
		return nil
	}
}

// Root finalizes the trie, emits all remaining node rows via the sink, and
// returns the trie root hash.
//
// Empty trie (no leaves): emits ([], 0x80) and returns EMPTY_TRIE_HASH.
func (b *Builder) Root() (common.Hash, error) {
	if b.leafCount == 0 {
		if err := b.sink([]byte{}, []byte{0x80}); err != nil {
			return common.Hash{}, err
		}
		return common.HexToHash(EmptyTrieHashHex), nil
	}

	if len(b.stack) == 0 {
		// Single-leaf trie: the root is the leaf itself.
		leafRLP, err := b.emitLeafRows([]byte{}, b.prevKey, b.prevVal)
		if err != nil {
			return common.Hash{}, err
		}
		return rlpToRootHash(leafRLP), nil
	}

	// Commit the final open leaf to the top branch.
	top := b.stack[len(b.stack)-1]
	prevLeafRLP, err := b.emitLeafRows(b.prevKey[:top.depth+1], b.prevKey[top.depth+1:], b.prevVal)
	if err != nil {
		return common.Hash{}, err
	}
	top.children[b.prevKey[top.depth]] = prevLeafRLP

	// Fold the whole spine up to the root.
	var rootRLP []byte
	for len(b.stack) > 0 {
		p := b.stack[len(b.stack)-1]
		b.stack = b.stack[:len(b.stack)-1]
		parentDepth := -1
		if len(b.stack) > 0 {
			parentDepth = b.stack[len(b.stack)-1].depth
		}
		pref, err := b.finalizeBranch(p, parentDepth)
		if err != nil {
			return common.Hash{}, err
		}
		if len(b.stack) > 0 {
			parent := b.stack[len(b.stack)-1]
			parent.children[b.prevKey[parent.depth]] = pref
		} else {
			rootRLP = pref
		}
	}
	return rlpToRootHash(rootRLP), nil
}

// finalizeBranch encodes and emits a branch node (and, when it sits below its
// parent via a shared prefix, the wrapping extension node), returning the node
// reference the parent should store. parentDepth is the nibble depth of the
// node that will reference this branch (-1 for the root). prevKey supplies the
// path nibbles; it is always a key contained in this branch's subtree.
func (b *Builder) finalizeBranch(p *openBranch, parentDepth int) ([]byte, error) {
	branchRLP := EncodeBranch(p.children, nil)
	if err := b.sink(cloneNibbles(b.prevKey[:p.depth]), branchRLP); err != nil {
		return nil, err
	}
	if extLen := p.depth - parentDepth - 1; extLen > 0 {
		prefix := b.prevKey[parentDepth+1 : p.depth]
		extRLP := EncodeExtension(prefix, branchRLP)
		if err := b.sink(cloneNibbles(b.prevKey[:parentDepth+1]), extRLP); err != nil {
			return nil, err
		}
		return extRLP, nil
	}
	return branchRLP, nil
}

// emitLeafRows emits the two leaf rows (node RLP row + full-path row) and returns
// the leaf node RLP. path is the nibble path to the leaf node; rem is the
// remaining path stored in the leaf; value is the raw leaf value.
func (b *Builder) emitLeafRows(path, rem, value []byte) ([]byte, error) {
	nodeRLP := EncodeLeaf(rem, value)

	// Row 1: (path, node RLP)
	if err := b.sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}

	// Row 2: (path ++ rem ++ [LeafFlag], raw value)
	fullPath := make([]byte, len(path)+len(rem)+1)
	copy(fullPath, path)
	copy(fullPath[len(path):], rem)
	fullPath[len(fullPath)-1] = LeafFlag
	if err := b.sink(fullPath, value); err != nil {
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
