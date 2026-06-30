package trie

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/besu"
	besurlp "github.com/ethereum/state-actor/internal/besu/rlp"
)

type acctEntry struct {
	hash common.Hash
	rlp  []byte
}

// makeSortedAccounts builds n distinct accounts sorted by addrHash ascending —
// the order the Phase 2 sorted-Pebble iteration feeds the builder.
func makeSortedAccounts(t *testing.T, n int) []acctEntry {
	t.Helper()
	out := make([]acctEntry, 0, n)
	for i := 0; i < n; i++ {
		h := crypto.Keccak256Hash([]byte{byte(i), byte(i >> 8), byte(i >> 16), 0xac})
		rlp, err := besurlp.EncodeAccount(uint64(i+1), uint256.NewInt(uint64(i)*7+1), besu.EmptyTrieNodeHash, besu.EmptyCodeHash)
		if err != nil {
			t.Fatalf("EncodeAccount: %v", err)
		}
		out = append(out, acctEntry{hash: h, rlp: rlp})
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].hash[:], out[j].hash[:]) < 0 })
	// Drop any (astronomically unlikely) duplicate hashes — the streaming
	// builder requires strictly ascending keys.
	dedup := out[:0]
	var last common.Hash
	for i, a := range out {
		if i > 0 && a.hash == last {
			continue
		}
		dedup = append(dedup, a)
		last = a.hash
	}
	return dedup
}

// assertAccountEquivalence builds the same sorted account set with the in-memory
// Builder (the trusted oracle) and the streaming builder, asserting a
// byte-identical trie: same root hash, same root RLP, and the same set of
// emitted nodes.
func assertAccountEquivalence(t *testing.T, accts []acctEntry) {
	t.Helper()

	// Reference: non-streaming in-memory Builder.
	refSink := &recordingSink{}
	ref := New(refSink)
	for _, a := range accts {
		if err := ref.AddAccount(a.hash, a.rlp); err != nil {
			t.Fatalf("ref AddAccount: %v", err)
		}
	}
	refHash, refRLP, err := ref.Commit()
	if err != nil {
		t.Fatalf("ref Commit: %v", err)
	}

	// Under test: streaming builder.
	strSink := &recordingSink{}
	str := NewStreamingAccountBuilder(strSink)
	for _, a := range accts {
		if err := str.AddAccount(a.hash, a.rlp); err != nil {
			t.Fatalf("streaming AddAccount: %v", err)
		}
	}
	strHash, strRLP, err := str.Commit()
	if err != nil {
		t.Fatalf("streaming Commit: %v", err)
	}

	if refHash != strHash {
		t.Errorf("root hash mismatch: ref %x, streaming %x", refHash, strHash)
	}
	if !bytes.Equal(refRLP, strRLP) {
		t.Errorf("root RLP mismatch: ref %x, streaming %x", refRLP, strRLP)
	}
	assertSameStateNodes(t, refSink.stateNodes, strSink.stateNodes)
}

// TestStreamingAccountBuilder_MatchesInMemoryBuilder is the correctness gate for
// the OOM fix: the streaming account builder must produce a byte-identical trie
// — same root hash, same root RLP, and the same set of emitted nodes — as the
// non-streaming in-memory Builder, for the same sorted account set.
func TestStreamingAccountBuilder_MatchesInMemoryBuilder(t *testing.T) {
	for _, n := range []int{0, 1, 2, 17, 256, 5000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			assertAccountEquivalence(t, makeSortedAccounts(t, n))
		})
	}
}

// TestStreamingAccountBuilder_ContractLeaves feeds contract-shaped account
// leaves (non-empty storageRoot + codeHash). makeSortedAccounts only produces
// EOA-shaped leaves (empty storage/code); the builder treats the leaf value as
// opaque bytes, so this pins that contract leaves stream identically to the
// oracle without relying on the Docker E2E suite.
func TestStreamingAccountBuilder_ContractLeaves(t *testing.T) {
	const n = 512
	accts := make([]acctEntry, 0, n)
	for i := 0; i < n; i++ {
		h := crypto.Keccak256Hash([]byte{byte(i), byte(i >> 8), 0xc0, 0x17})
		storageRoot, codeHash := besu.EmptyTrieNodeHash, besu.EmptyCodeHash
		if i%3 == 0 {
			// Distinct non-empty roots/hashes — opaque to the builder, need not
			// be real trie roots.
			storageRoot = crypto.Keccak256Hash([]byte{byte(i), 0x5a})
			codeHash = crypto.Keccak256Hash([]byte{byte(i), 0xc0})
		}
		rlp, err := besurlp.EncodeAccount(uint64(i+1), uint256.NewInt(uint64(i)+1), storageRoot, codeHash)
		if err != nil {
			t.Fatalf("EncodeAccount: %v", err)
		}
		accts = append(accts, acctEntry{hash: h, rlp: rlp})
	}
	sort.Slice(accts, func(i, j int) bool { return bytes.Compare(accts[i].hash[:], accts[j].hash[:]) < 0 })
	assertAccountEquivalence(t, accts)
}

// TestStreamingAccountBuilder_RootExtension pins the one root shape the random-
// key equivalence test never produces: a root that is an EXTENSION node. All
// keys share a long leading prefix (30 identical bytes, distinct 2-byte suffix),
// so the trie root is an extension over the shared prefix. This exercises the
// streaming builder's root-RLP capture for an extension root.
func TestStreamingAccountBuilder_RootExtension(t *testing.T) {
	const n = 64
	accts := make([]acctEntry, 0, n)
	for i := 0; i < n; i++ {
		var h common.Hash
		for k := 0; k < 30; k++ {
			h[k] = 0xAB // shared 30-byte (60-nibble) prefix → extension root
		}
		h[30] = byte(i >> 8)
		h[31] = byte(i)
		rlp, err := besurlp.EncodeAccount(uint64(i+1), uint256.NewInt(uint64(i)+1), besu.EmptyTrieNodeHash, besu.EmptyCodeHash)
		if err != nil {
			t.Fatalf("EncodeAccount: %v", err)
		}
		accts = append(accts, acctEntry{hash: h, rlp: rlp})
	}
	sort.Slice(accts, func(i, j int) bool { return bytes.Compare(accts[i].hash[:], accts[j].hash[:]) < 0 })
	assertAccountEquivalence(t, accts)
}

// TestStreamingAccountBuilder_RejectsOutOfOrder pins the strict-ascending input
// contract.
func TestStreamingAccountBuilder_RejectsOutOfOrder(t *testing.T) {
	b := NewStreamingAccountBuilder(&recordingSink{})
	hi := crypto.Keccak256Hash([]byte{0x02})
	lo := crypto.Keccak256Hash([]byte{0x01})
	if hi.Big().Cmp(lo.Big()) < 0 {
		hi, lo = lo, hi // ensure hi > lo
	}
	if err := b.AddAccount(hi, []byte{0x11}); err != nil {
		t.Fatalf("first AddAccount: %v", err)
	}
	if err := b.AddAccount(lo, []byte{0x22}); !errors.Is(err, ErrSlotsOutOfOrder) {
		t.Fatalf("expected ErrSlotsOutOfOrder for descending addrHash, got %v", err)
	}
}

// assertSameStateNodes checks the two builders emitted the same set of account
// trie nodes (keyed by location). Emission ORDER may differ (in-memory commits
// post-order at the end; streaming emits as the spine collapses), so compare as
// a set.
func assertSameStateNodes(t *testing.T, ref, got []sinkRecord) {
	t.Helper()
	if len(ref) != len(got) {
		t.Errorf("emitted node count: ref %d, streaming %d", len(ref), len(got))
	}
	index := func(rs []sinkRecord) map[string]sinkRecord {
		m := make(map[string]sinkRecord, len(rs))
		for _, r := range rs {
			m[string(r.location)] = r
		}
		return m
	}
	refByLoc := index(ref)
	for _, g := range got {
		r, ok := refByLoc[string(g.location)]
		if !ok {
			t.Errorf("streaming emitted node at location %x absent in ref", g.location)
			continue
		}
		if r.hash != g.hash || !bytes.Equal(r.rlp, g.rlp) {
			t.Errorf("node mismatch at location %x:\n  ref:       hash %x rlp %x\n  streaming: hash %x rlp %x",
				g.location, r.hash, r.rlp, g.hash, g.rlp)
		}
	}
}
