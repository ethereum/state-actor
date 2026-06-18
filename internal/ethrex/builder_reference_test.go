package ethrex

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// builder_reference_test.go pins the streaming Builder to a recursive
// build-then-emit reference. The reference is the pre-streaming algorithm:
// buffer all leaves, build the whole trie in memory, then emit it depth-first.
// A randomized differential test asserts the streaming Builder emits a
// byte-identical row set and the same root over thousands of random key sets,
// so the streaming rewrite is provably output-equivalent.

// ---------------------------------------------------------------------------
// Recursive reference implementation (the former in-memory Builder internals).
// ---------------------------------------------------------------------------

type refEntry struct {
	key   []byte
	value []byte
}

type refNode interface{ isRefNode() }

type refLeaf struct {
	rem   []byte
	value []byte
}
type refExtension struct {
	prefix []byte
	child  refNode
}
type refBranch struct {
	children [16]refNode
	value    []byte
}

func (refLeaf) isRefNode()      {}
func (refExtension) isRefNode() {}
func (*refBranch) isRefNode()   {}

func refBuildNode(entries []refEntry, depth int) refNode {
	if len(entries) == 1 {
		e := entries[0]
		return refLeaf{rem: e.key[depth:], value: e.value}
	}
	maxCPL := len(entries[0].key) - depth
	for _, e := range entries[1:] {
		c := commonPrefixLen(entries[0].key[depth:], e.key[depth:])
		if c < maxCPL {
			maxCPL = c
		}
	}
	splitAt := depth + maxCPL
	var branch refBranch
	start := 0
	for start < len(entries) {
		d := entries[start].key[splitAt]
		end := start + 1
		for end < len(entries) && entries[end].key[splitAt] == d {
			end++
		}
		branch.children[d] = refBuildNode(entries[start:end], splitAt+1)
		start = end
	}
	if maxCPL > 0 {
		return refExtension{prefix: entries[0].key[depth : depth+maxCPL], child: &branch}
	}
	return &branch
}

func refEmitNode(node refNode, path []byte, sink NodeSink) ([]byte, error) {
	switch n := node.(type) {
	case refLeaf:
		return refEmitLeaf(n, path, sink)
	case *refBranch:
		return refEmitBranch(n, path, sink)
	case refExtension:
		return refEmitExtension(n, path, sink)
	default:
		panic("unknown ref node type")
	}
}

func refEmitLeaf(n refLeaf, path []byte, sink NodeSink) ([]byte, error) {
	nodeRLP := EncodeLeaf(n.rem, n.value)
	if err := sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}
	fullPath := make([]byte, len(path)+len(n.rem)+1)
	copy(fullPath, path)
	copy(fullPath[len(path):], n.rem)
	fullPath[len(fullPath)-1] = LeafFlag
	if err := sink(fullPath, n.value); err != nil {
		return nil, err
	}
	return nodeRLP, nil
}

func refEmitBranch(n *refBranch, path []byte, sink NodeSink) ([]byte, error) {
	var children [16][]byte
	for i, child := range n.children {
		if child == nil {
			continue
		}
		childPath := make([]byte, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = byte(i)
		childRLP, err := refEmitNode(child, childPath, sink)
		if err != nil {
			return nil, err
		}
		children[i] = childRLP
	}
	nodeRLP := EncodeBranch(children, n.value)
	if err := sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}
	return nodeRLP, nil
}

func refEmitExtension(n refExtension, path []byte, sink NodeSink) ([]byte, error) {
	childPath := make([]byte, len(path)+len(n.prefix))
	copy(childPath, path)
	copy(childPath[len(path):], n.prefix)
	childRLP, err := refEmitNode(n.child, childPath, sink)
	if err != nil {
		return nil, err
	}
	nodeRLP := EncodeExtension(n.prefix, childRLP)
	if err := sink(cloneNibbles(path), nodeRLP); err != nil {
		return nil, err
	}
	return nodeRLP, nil
}

// referenceBuild runs the recursive reference over sorted (keys, vals).
func referenceBuild(keys, vals [][]byte, sink NodeSink) (common.Hash, error) {
	if len(keys) == 0 {
		if err := sink([]byte{}, []byte{0x80}); err != nil {
			return common.Hash{}, err
		}
		return common.HexToHash(EmptyTrieHashHex), nil
	}
	entries := make([]refEntry, len(keys))
	for i := range keys {
		entries[i] = refEntry{key: keys[i], value: vals[i]}
	}
	root := refBuildNode(entries, 0)
	nodeRLP, err := refEmitNode(root, nil, sink)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(nodeRLP), nil
}

// ---------------------------------------------------------------------------
// Differential test
// ---------------------------------------------------------------------------

// rowMultiset collects emitted (path, value) rows into a multiset keyed by
// hex(path)|hex(value), so the two emission orders can be compared order-free.
func collectRows() (NodeSink, map[string]int) {
	rows := map[string]int{}
	sink := func(path, val []byte) error {
		rows[fmt.Sprintf("%x|%x", path, val)]++
		return nil
	}
	return sink, rows
}

func TestBuilderStreamingMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(0xB17EC0DE))

	// A spread of nibble lengths. Short lengths pack the keyspace densely, which
	// maximizes extensions, splits and deep folds — the tricky spine cases.
	lengths := []int{1, 2, 3, 4, 5, 8, 16, 32, 64}

	for iter := 0; iter < 4000; iter++ {
		L := lengths[rng.Intn(len(lengths))]
		count := rng.Intn(64) // 0..63 leaves
		keys, vals := randSortedKV(rng, count, L)

		streamSink, streamRows := collectRows()
		sb := NewBuilder(streamSink)
		for i := range keys {
			if err := sb.AddLeaf(keys[i], vals[i]); err != nil {
				t.Fatalf("iter %d (L=%d n=%d): AddLeaf: %v", iter, L, len(keys), err)
			}
		}
		streamRoot, err := sb.Root()
		if err != nil {
			t.Fatalf("iter %d: streaming Root: %v", iter, err)
		}

		refSink, refRows := collectRows()
		refRoot, err := referenceBuild(keys, vals, refSink)
		if err != nil {
			t.Fatalf("iter %d: reference: %v", iter, err)
		}

		if streamRoot != refRoot {
			t.Fatalf("iter %d (L=%d n=%d): root mismatch\n stream=%s\n    ref=%s\n keys=%s",
				iter, L, len(keys), streamRoot.Hex(), refRoot.Hex(), fmtKeys(keys))
		}
		if !equalMultiset(streamRows, refRows) {
			t.Fatalf("iter %d (L=%d n=%d): emitted row set mismatch\n%s\n keys=%s",
				iter, L, len(keys), diffMultiset(streamRows, refRows), fmtKeys(keys))
		}
	}
}

func randSortedKV(rng *rand.Rand, n, L int) (keys, vals [][]byte) {
	set := map[string]struct{}{}
	for tries := 0; len(set) < n && tries < n*200+50; tries++ {
		k := make([]byte, L)
		for i := range k {
			k[i] = byte(rng.Intn(16))
		}
		set[string(k)] = struct{}{}
	}
	keys = make([][]byte, 0, len(set))
	for s := range set {
		keys = append(keys, []byte(s))
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	vals = make([][]byte, len(keys))
	for i := range keys {
		v := make([]byte, 1+rng.Intn(40))
		for j := range v {
			v[j] = byte(rng.Intn(256))
		}
		vals[i] = v
	}
	return keys, vals
}

func equalMultiset(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func diffMultiset(stream, ref map[string]int) string {
	var sb bytes.Buffer
	for k, v := range stream {
		if ref[k] != v {
			fmt.Fprintf(&sb, "  stream-only/extra %dx %s\n", v-ref[k], k)
		}
	}
	for k, v := range ref {
		if _, ok := stream[k]; !ok {
			fmt.Fprintf(&sb, "  ref-only %dx %s\n", v, k)
		}
	}
	return sb.String()
}

func fmtKeys(keys [][]byte) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%x", k)
	}
	return fmt.Sprint(parts)
}
