package trie

import (
	"bytes"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/besu"
	besurlp "github.com/ethereum/state-actor/internal/besu/rlp"
)

// recordingSink is a NodeSink that captures all writes for assertion.
type recordingSink struct {
	stateNodes   []sinkRecord
	storageNodes []sinkStorageRecord
	saved        *sinkSaveRecord
	failOn       int // if > 0, return error on the Nth state-node write
	count        int
}

type sinkRecord struct {
	location []byte
	hash     common.Hash
	rlp      []byte
}

type sinkStorageRecord struct {
	addrHash common.Hash
	location []byte
	hash     common.Hash
	rlp      []byte
}

type sinkSaveRecord struct {
	blockHash common.Hash
	rootHash  common.Hash
	rootRLP   []byte
}

func (s *recordingSink) PutAccountStateTrieNode(location []byte, hash common.Hash, rlp []byte) error {
	s.count++
	if s.failOn > 0 && s.count == s.failOn {
		return errFailingSink
	}
	s.stateNodes = append(s.stateNodes, sinkRecord{
		location: append([]byte(nil), location...),
		hash:     hash,
		rlp:      append([]byte(nil), rlp...),
	})
	return nil
}

func (s *recordingSink) PutAccountStorageTrieNode(addrHash common.Hash, location []byte, hash common.Hash, rlp []byte) error {
	s.storageNodes = append(s.storageNodes, sinkStorageRecord{
		addrHash: addrHash,
		location: append([]byte(nil), location...),
		hash:     hash,
		rlp:      append([]byte(nil), rlp...),
	})
	return nil
}

func (s *recordingSink) SaveWorldState(blockHash, rootHash common.Hash, rootRLP []byte) error {
	s.saved = &sinkSaveRecord{
		blockHash: blockHash,
		rootHash:  rootHash,
		rootRLP:   append([]byte(nil), rootRLP...),
	}
	return nil
}

var errFailingSink = &fakeErr{"failing sink"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

// TestBuilder_EmptyTrie pins zero-account behavior: returns EmptyTrieNodeHash,
// no sink calls.
func TestBuilder_EmptyTrie(t *testing.T) {
	sink := &recordingSink{}
	b := New(sink)
	rootHash, rootRLP, err := b.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if rootHash != besu.EmptyTrieNodeHash {
		t.Fatalf("empty root: got %x, want %x", rootHash, besu.EmptyTrieNodeHash)
	}
	if !bytes.Equal(rootRLP, []byte{0x80}) {
		t.Fatalf("empty rootRLP: got %x, want [0x80]", rootRLP)
	}
	if len(sink.stateNodes) != 0 {
		t.Fatalf("empty trie emitted %d nodes, want 0", len(sink.stateNodes))
	}
}

// TestBuilder_TwoAccounts_Genesis1Root is the load-bearing parity test.
//
// Replays the alloc from Besu's `genesis1.json` (2 accounts: addr 0x...01
// balance 111111111, addr 0x...02 balance 222222222, both EOAs) and asserts
// the computed state root equals the value pinned by Besu's own
// `GenesisStateTest.java:74-75` test.
//
// Source hash: 0x92683e6af0f8a932e5fe08c870f2ae9d287e39d4518ec544b0be451f1035fd39
//
// Bonsai and Forest share standard Ethereum MPT root computation; only the
// on-disk node-storage layout differs. So this golden hash applies to our
// Go builder regardless of the path-keyed DB encoding.
func TestBuilder_TwoAccounts_Genesis1Root(t *testing.T) {
	type alloc struct {
		addr    common.Address
		balance string
	}
	allocs := []alloc{
		{common.HexToAddress("0x0000000000000000000000000000000000000001"), "111111111"},
		{common.HexToAddress("0x0000000000000000000000000000000000000002"), "222222222"},
	}

	type entry struct {
		addrHash common.Hash
		rlp      []byte
	}
	entries := make([]entry, 0, len(allocs))
	for _, a := range allocs {
		balance, err := uint256.FromDecimal(a.balance)
		if err != nil {
			t.Fatalf("parse balance: %v", err)
		}
		accRLP, err := besurlp.EncodeAccount(0, balance, besu.EmptyTrieNodeHash, besu.EmptyCodeHash)
		if err != nil {
			t.Fatalf("EncodeAccount: %v", err)
		}
		entries = append(entries, entry{
			addrHash: crypto.Keccak256Hash(a.addr[:]),
			rlp:      accRLP,
		})
	}
	// Sort by addrHash ascending (matches Phase 2 streaming order).
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].addrHash[:], entries[j].addrHash[:]) < 0
	})

	sink := &recordingSink{}
	b := New(sink)
	for _, e := range entries {
		if err := b.AddAccount(e.addrHash, e.rlp); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}
	rootHash, _, err := b.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	want := common.HexToHash("0x92683e6af0f8a932e5fe08c870f2ae9d287e39d4518ec544b0be451f1035fd39")
	if rootHash != want {
		t.Fatalf("genesis1 stateRoot:\n  got:  %x\n  want: %x", rootHash, want)
	}
}

// TestBuilder_Reproducibility verifies that the same input order produces
// byte-identical sink call sequences and root hashes.
func TestBuilder_Reproducibility(t *testing.T) {
	type entry struct {
		addrHash common.Hash
		rlp      []byte
	}
	build := func() (common.Hash, []sinkRecord) {
		entries := []entry{
			{crypto.Keccak256Hash(common.HexToAddress("0x01").Bytes()), []byte{0x11}},
			{crypto.Keccak256Hash(common.HexToAddress("0x02").Bytes()), []byte{0x22}},
			{crypto.Keccak256Hash(common.HexToAddress("0x03").Bytes()), []byte{0x33}},
		}
		sort.Slice(entries, func(i, j int) bool {
			return bytes.Compare(entries[i].addrHash[:], entries[j].addrHash[:]) < 0
		})
		sink := &recordingSink{}
		b := New(sink)
		for _, e := range entries {
			_ = b.AddAccount(e.addrHash, e.rlp)
		}
		root, _, _ := b.Commit()
		return root, sink.stateNodes
	}
	root1, calls1 := build()
	root2, calls2 := build()
	if root1 != root2 {
		t.Fatalf("non-reproducible root: %x vs %x", root1, root2)
	}
	if len(calls1) != len(calls2) {
		t.Fatalf("non-reproducible sink call count: %d vs %d", len(calls1), len(calls2))
	}
	for i := range calls1 {
		if !bytes.Equal(calls1[i].location, calls2[i].location) {
			t.Fatalf("call[%d].location differs: %x vs %x", i, calls1[i].location, calls2[i].location)
		}
		if calls1[i].hash != calls2[i].hash {
			t.Fatalf("call[%d].hash differs: %x vs %x", i, calls1[i].hash, calls2[i].hash)
		}
		if !bytes.Equal(calls1[i].rlp, calls2[i].rlp) {
			t.Fatalf("call[%d].rlp differs", i)
		}
	}
}

// TestBuilder_OrderInvariance verifies that different insertion orders produce
// the same root hash. The sink call sequence may differ but the root must not.
func TestBuilder_OrderInvariance(t *testing.T) {
	addrs := []common.Address{
		common.HexToAddress("0x01"),
		common.HexToAddress("0x02"),
		common.HexToAddress("0x03"),
		common.HexToAddress("0x04"),
		common.HexToAddress("0x05"),
	}
	values := [][]byte{{0x11}, {0x22}, {0x33}, {0x44}, {0x55}}
	addrHash := func(a common.Address) common.Hash { return crypto.Keccak256Hash(a[:]) }

	// Forward order.
	bA := New(&recordingSink{})
	for i, a := range addrs {
		_ = bA.AddAccount(addrHash(a), values[i])
	}
	rootA, _, _ := bA.Commit()

	// Reverse order.
	bB := New(&recordingSink{})
	for i := len(addrs) - 1; i >= 0; i-- {
		_ = bB.AddAccount(addrHash(addrs[i]), values[i])
	}
	rootB, _, _ := bB.Commit()

	if rootA != rootB {
		t.Fatalf("root not order-invariant: forward=%x reverse=%x", rootA, rootB)
	}
}

// TestBuilder_LocationByteRange verifies that every emitted location has all
// bytes in [0x00, 0x0F]. This is the load-bearing path-encoding invariant
// (one nibble per byte, NOT packed).
func TestBuilder_LocationByteRange(t *testing.T) {
	sink := &recordingSink{}
	b := New(sink)
	for i := 0; i < 10; i++ {
		addr := common.HexToAddress("0x" + repeat(byte('0'+(i%10)), 40))
		_ = b.AddAccount(crypto.Keccak256Hash(addr[:]), []byte{byte(i)})
	}
	_, _, err := b.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, r := range sink.stateNodes {
		for j, byteVal := range r.location {
			if byteVal > 0x0f {
				t.Fatalf("location[%d]=%#x out of nibble range (writes are NOT packed): full=%x",
					j, byteVal, r.location)
			}
		}
	}
}

func repeat(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// TestBuilder_SinkErrorPropagation verifies that a sink error during commit
// surfaces from Commit().
func TestBuilder_SinkErrorPropagation(t *testing.T) {
	sink := &recordingSink{failOn: 1}
	b := New(sink)
	_ = b.AddAccount(crypto.Keccak256Hash([]byte{0x01}), []byte{0x11})
	_ = b.AddAccount(crypto.Keccak256Hash([]byte{0x02}), []byte{0x22})
	_, _, err := b.Commit()
	if err == nil {
		t.Fatal("expected sink error to surface from Commit()")
	}
}

// TestStorageBuilder_Empty pins zero-slot behavior: returns EmptyTrieNodeHash
// with no sink calls.
func TestStorageBuilder_Empty(t *testing.T) {
	sink := &recordingSink{}
	parent := New(sink)
	addrHash := crypto.Keccak256Hash([]byte{0xab})
	sb := parent.BeginStorage(addrHash)
	root, err := sb.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if root != besu.EmptyTrieNodeHash {
		t.Fatalf("empty storage root: got %x, want %x", root, besu.EmptyTrieNodeHash)
	}
	if len(sink.storageNodes) != 0 {
		t.Fatalf("empty storage trie emitted %d nodes, want 0", len(sink.storageNodes))
	}
}

// TestStorageBuilder_OneSlot verifies storage trie produces a non-empty root
// and at least one storage-node sink call when needed.
func TestStorageBuilder_OneSlot(t *testing.T) {
	sink := &recordingSink{}
	parent := New(sink)
	addrHash := crypto.Keccak256Hash([]byte{0xab})
	sb := parent.BeginStorage(addrHash)
	slotHash := crypto.Keccak256Hash([]byte{0x01})
	if err := sb.AddSlot(slotHash, []byte{0xcd}); err != nil {
		t.Fatalf("AddSlot: %v", err)
	}
	root, err := sb.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if root == besu.EmptyTrieNodeHash {
		t.Fatal("non-empty storage trie returned EmptyTrieNodeHash")
	}
}

// TestStorageBuilder_KeyPrefix verifies that storage-trie sink calls carry
// the addrHash prefix (the DB key would be addrHash(32) ++ location).
func TestStorageBuilder_KeyPrefix(t *testing.T) {
	sink := &recordingSink{}
	parent := New(sink)
	addrHash := crypto.Keccak256Hash([]byte{0xab})
	sb := parent.BeginStorage(addrHash)
	for i := 0; i < 5; i++ {
		_ = sb.AddSlot(crypto.Keccak256Hash([]byte{byte(i)}), []byte{byte(0xa0 + i)})
	}
	if _, err := sb.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, r := range sink.storageNodes {
		if r.addrHash != addrHash {
			t.Fatalf("storage node addrHash: got %x, want %x", r.addrHash, addrHash)
		}
	}
}
