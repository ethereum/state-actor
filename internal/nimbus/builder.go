package nimbus

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/ethrex"
)

// ErrKeysOutOfOrder is returned by AddLeaf when a key is not strictly greater
// than the previous one (nibble-lexicographic order).
var ErrKeysOutOfOrder = errors.New("nimbus.Builder: keys must be inserted in strictly ascending nibble order")

// ErrVertexTooLarge is returned when a record would exceed MaxVertexBlobSize.
// Unreachable for 64-nibble keys (the largest AccLeaf is exactly 118 bytes).
var ErrVertexTooLarge = errors.New("nimbus.Builder: vertex record exceeds MaxVertexBlobSize")

// Sink receives one finished vertex row. record is borrowed scratch that the
// Builder reuses across calls — copy it if it must outlive the call.
type Sink func(rvid RootedVertexID, record []byte) error

// openBranch is a branch on the rightmost spine that may still gain children.
// depth is the nibble index it splits on (its children sit at position
// depth+1); startVid is decided when the branch is created and never changes,
// because a later extension split moves the branch's own position but not
// its children's. children holds the node RLP of already-finalized children
// for hashing; the open child is implicit.
type openBranch struct {
	depth    int
	startVid VertexID
	used     uint16
	children [16][]byte
}

// Builder is a streaming Merkle-Patricia trie builder that emits Aristo vertex
// records for one trie (the account trie rooted at StateRootVID, or a storage
// trie rooted at its stoID). It is internal/ethrex.Builder's stack-trie spine
// with three additions: every branch owns a startVid (StaticStartVid for child
// positions <= StaticVIDLevels, else a 16-block from the VidAllocator), every
// emitted vertex is keyed parent.startVid + nibble (the root at (root, root)),
// and an MPT extension collapses into the ExtBranch record of the branch
// under it. Branch records carry the node's 32-byte Merkle key whenever the
// referenced RLP is >= 32 bytes.
//
// Leaves must be added in strictly ascending nibble order, all of one length
// (64 in production). Memory is O(keyLen): the spine plus the last leaf.
type Builder struct {
	root  VertexID
	alloc *VidAllocator
	sink  Sink

	started  bool
	finished bool
	keyLen   int

	prevKey     []byte
	prevVal     []byte // node value RLP of the open leaf (for hashing)
	prevPayload []byte // record payload of the open leaf (for the row)
	stack       []*openBranch

	leafCount    int
	minLeafDepth int

	rec    []byte
	hasher crypto.KeccakState
	key32  [32]byte
}

// NewBuilder returns a Builder for the trie rooted at root. sink may be nil
// when only the root hash is wanted.
func NewBuilder(root VertexID, alloc *VidAllocator, sink Sink) *Builder {
	if root == 0 {
		panic("nimbus.NewBuilder: root vertex ID must be non-zero")
	}
	if alloc == nil {
		panic("nimbus.NewBuilder: nil VidAllocator")
	}
	if sink == nil {
		sink = func(RootedVertexID, []byte) error { return nil }
	}
	return &Builder{
		root:         root,
		alloc:        alloc,
		sink:         sink,
		keyLen:       -1,
		minLeafDepth: math.MaxInt,
		rec:          make([]byte, 0, MaxVertexBlobSize+64),
		hasher:       crypto.NewKeccakState(),
	}
}

// AddLeaf inserts one leaf. keyNibbles is the full nibble path; valueRLP is
// the leaf's value as it enters the node RLP (account RLP / RLP(slot value));
// leafPayload is the record payload (AppendAccLeafPayload /
// AppendStoLeafPayload output). None of the arguments are retained.
func (b *Builder) AddLeaf(keyNibbles, valueRLP, leafPayload []byte) error {
	if b.finished {
		return errors.New("nimbus.Builder: AddLeaf after Root")
	}
	if !b.started {
		if len(keyNibbles) == 0 {
			return errors.New("nimbus.Builder: empty key")
		}
		b.started = true
		b.keyLen = len(keyNibbles)
		b.leafCount = 1
		b.setPrev(keyNibbles, valueRLP, leafPayload)
		return nil
	}
	if bytes.Compare(keyNibbles, b.prevKey) <= 0 {
		return ErrKeysOutOfOrder
	}
	if len(keyNibbles) != b.keyLen {
		return fmt.Errorf("nimbus.Builder: all keys must have the same nibble length (got %d, want %d)", len(keyNibbles), b.keyLen)
	}
	if err := b.insert(keyNibbles); err != nil {
		return err
	}
	b.leafCount++
	b.setPrev(keyNibbles, valueRLP, leafPayload)
	return nil
}

func (b *Builder) setPrev(key, val, payload []byte) {
	b.prevKey = append(b.prevKey[:0], key...)
	b.prevVal = append(b.prevVal[:0], val...)
	b.prevPayload = append(b.prevPayload[:0], payload...)
}

// LeafCount is the number of leaves added so far.
func (b *Builder) LeafCount() int { return b.leafCount }

// MinLeafDepth is the shallowest leaf position (nibbles consumed before the
// leaf). Meaningful only after Root(), when every leaf has been placed.
func (b *Builder) MinLeafDepth() int { return b.minLeafDepth }

// StoHint is the AccLeaf hint for this trie when used as a storage trie:
// min(MinLeafDepth, StaticVIDLevels)+1, i.e. the reader starts probing at the
// shallowest leaf level (nimbus biases its own heuristic shallow too: a miss
// below the leaves costs one negative lookup per level, a hit above lands on
// a branch the walk resolves from). 0 for an empty trie. Call after Root().
func (b *Builder) StoHint() byte {
	if b.leafCount == 0 {
		return 0
	}
	d := b.minLeafDepth
	if d > StaticVIDLevels {
		d = StaticVIDLevels
	}
	return byte(d) + 1
}

// childStartVid returns the startVid for a branch whose children sit at
// position childPos. prevKey is always inside that branch's subtree, so its
// prefix is the branch's path.
func (b *Builder) childStartVid(childPos int) VertexID {
	if childPos <= StaticVIDLevels {
		return StaticStartVid(b.prevKey, childPos)
	}
	return b.alloc.Alloc(16)
}

func (b *Builder) newOpenBranch(depth int) *openBranch {
	return &openBranch{depth: depth, startVid: b.childStartVid(depth + 1)}
}

// vidUnder is the vertex ID of the spine node whose parent is parent (nil for
// the root). Every node this Builder emits contains prevKey.
func (b *Builder) vidUnder(parent *openBranch) VertexID {
	if parent == nil {
		return b.root
	}
	return parent.startVid + VertexID(b.prevKey[parent.depth])
}

func (b *Builder) attach(parent *openBranch, ref []byte) {
	nib := b.prevKey[parent.depth]
	parent.children[nib] = ref
	parent.used |= 1 << nib
}

func (b *Builder) top() *openBranch { return b.stack[len(b.stack)-1] }

func (b *Builder) pop() *openBranch {
	p := b.stack[len(b.stack)-1]
	b.stack[len(b.stack)-1] = nil
	b.stack = b.stack[:len(b.stack)-1]
	return p
}

// insert folds the previous (now closed) leaf into the spine and positions
// the spine to receive key as the new open leaf.
func (b *Builder) insert(key []byte) error {
	lcp := commonPrefixLen(b.prevKey, key)
	dTop := -1
	if len(b.stack) > 0 {
		dTop = b.top().depth
	}
	d := lcp
	if dTop > d {
		d = dTop
	}

	if d > dTop {
		// The previous and new leaf share more than the deepest branch covers:
		// they form a new branch at d, created BEFORE the leaf is emitted so the
		// leaf's vertex ID (startVid + nibble) is known.
		nb := b.newOpenBranch(d)
		ref, err := b.emitLeaf(nb)
		if err != nil {
			return err
		}
		b.attach(nb, ref)
		b.stack = append(b.stack, nb)
		return nil
	}

	// d == dTop: the previous leaf is a child of the top branch.
	top := b.top()
	ref, err := b.emitLeaf(top)
	if err != nil {
		return err
	}
	b.attach(top, ref)
	if lcp == dTop {
		return nil
	}
	return b.foldTo(lcp)
}

// foldTo finalizes every spine branch deeper than lcp, attaching each to its
// parent, and leaves a branch at depth lcp on top of the stack to receive the
// new key. When no branch exists at lcp, one is created — before the child is
// finalized, so the child's vertex ID can be derived from it.
func (b *Builder) foldTo(lcp int) error {
	for {
		p := b.pop()
		var parent *openBranch
		if len(b.stack) > 0 && b.top().depth >= lcp {
			parent = b.top()
		} else {
			parent = b.newOpenBranch(lcp)
			b.stack = append(b.stack, parent)
		}
		ref, err := b.finalizeBranch(p, parent)
		if err != nil {
			return err
		}
		b.attach(parent, ref)
		if parent.depth == lcp {
			return nil
		}
	}
}

// emitLeaf writes the record of the open leaf (prevKey) as a child of parent
// and returns its node RLP for the parent's hash.
func (b *Builder) emitLeaf(parent *openBranch) ([]byte, error) {
	pos := 0
	if parent != nil {
		pos = parent.depth + 1
	}
	rem := b.prevKey[pos:]
	rec := append(b.rec[:0], b.prevPayload...)
	hpStart := len(rec)
	rec = AppendHexPrefix(rec, rem, true)
	hpLen := len(rec) - hpStart
	rec = append(rec, bitsLeaf<<6|byte(hpLen))
	b.rec = rec[:0]
	if len(rec) > MaxVertexBlobSize {
		return nil, fmt.Errorf("%w: leaf record is %d bytes", ErrVertexTooLarge, len(rec))
	}
	if err := b.sink(RootedVertexID{Root: b.root, Vid: b.vidUnder(parent)}, rec); err != nil {
		return nil, err
	}
	if pos < b.minLeafDepth {
		b.minLeafDepth = pos
	}
	return ethrex.EncodeLeaf(rem, b.prevVal), nil
}

// finalizeBranch writes the record of branch p as a child of parent and
// returns the RLP the parent references: the branch RLP, or — when p sits
// below parent through a shared prefix — the extension RLP wrapping it. Both
// forms are one ExtBranch/Branch record; the stored key is the keccak of the
// referenced RLP when that is 32 bytes or longer.
func (b *Builder) finalizeBranch(p, parent *openBranch) ([]byte, error) {
	parentDepth := -1
	if parent != nil {
		parentDepth = parent.depth
	}
	pfx := b.prevKey[parentDepth+1 : p.depth]
	branchRLP := ethrex.EncodeBranch(p.children, nil)
	ref := branchRLP
	if len(pfx) > 0 {
		ref = ethrex.EncodeExtension(pfx, branchRLP)
	}
	var key []byte
	if len(ref) >= 32 {
		b.hasher.Reset()
		b.hasher.Write(ref)
		b.hasher.Read(b.key32[:])
		key = b.key32[:]
	}
	rec := AppendBranch(b.rec[:0], key, p.startVid, p.used, pfx)
	b.rec = rec[:0]
	if len(rec) > MaxVertexBlobSize {
		return nil, fmt.Errorf("%w: branch record is %d bytes", ErrVertexTooLarge, len(rec))
	}
	if err := b.sink(RootedVertexID{Root: b.root, Vid: b.vidUnder(parent)}, rec); err != nil {
		return nil, err
	}
	return ref, nil
}

// Root emits every remaining vertex and returns the trie root hash. An empty
// trie emits nothing and returns EmptyRootHash; a single-leaf trie's root row
// is the leaf itself at (root, root).
func (b *Builder) Root() (common.Hash, error) {
	if b.finished {
		return common.Hash{}, errors.New("nimbus.Builder: Root called twice")
	}
	b.finished = true
	if b.leafCount == 0 {
		return EmptyRootHash, nil
	}
	if len(b.stack) == 0 {
		leafRLP, err := b.emitLeaf(nil)
		if err != nil {
			return common.Hash{}, err
		}
		return crypto.Keccak256Hash(leafRLP), nil
	}
	top := b.top()
	ref, err := b.emitLeaf(top)
	if err != nil {
		return common.Hash{}, err
	}
	b.attach(top, ref)

	var rootRef []byte
	for len(b.stack) > 0 {
		p := b.pop()
		var parent *openBranch
		if len(b.stack) > 0 {
			parent = b.top()
		}
		ref, err := b.finalizeBranch(p, parent)
		if err != nil {
			return common.Hash{}, err
		}
		if parent != nil {
			b.attach(parent, ref)
		} else {
			rootRef = ref
		}
	}
	return crypto.Keccak256Hash(rootRef), nil
}

// commonPrefixLen returns the number of leading nibbles a and b share.
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
