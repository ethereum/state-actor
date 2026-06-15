package trie

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/besu"
	besurlp "github.com/ethereum/state-actor/internal/besu/rlp"
)

// TestStreamingStorageBuilder_Empty: zero AddSlot → EmptyTrieNodeHash,
// no sink calls.
func TestStreamingStorageBuilder_Empty(t *testing.T) {
	sink := &recordingSink{}
	b := New(sink)
	sb := b.BeginStreamingStorage(common.HexToHash("0xa"))

	root, err := sb.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if root != besu.EmptyTrieNodeHash {
		t.Errorf("root = %s, want EmptyTrieNodeHash", root.Hex())
	}
	if len(sink.storageNodes) != 0 {
		t.Errorf("sink saw %d storage writes, want 0", len(sink.storageNodes))
	}
}

// TestStreamingStorageBuilder_RejectsOutOfOrder pins the input contract.
func TestStreamingStorageBuilder_RejectsOutOfOrder(t *testing.T) {
	sink := &recordingSink{}
	b := New(sink)
	sb := b.BeginStreamingStorage(common.HexToHash("0xa"))

	hi := common.HexToHash("0xff00000000000000000000000000000000000000000000000000000000000000")
	lo := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")

	value := besurlp.EncodeStorageValue(common.HexToHash("0x42"))
	if err := sb.AddSlot(hi, value); err != nil {
		t.Fatalf("first AddSlot: %v", err)
	}
	if err := sb.AddSlot(lo, value); !errors.Is(err, ErrSlotsOutOfOrder) {
		t.Errorf("second AddSlot: got %v, want ErrSlotsOutOfOrder", err)
	}
}

// TestStreamingStorageBuilder_ParityWithNonStreaming pins the streaming
// builder against the non-streaming StorageBuilder. Same input must
// yield: identical root hash AND identical (location, hash, rlp)
// emission multiset (sorted by location bytes for comparison —
// emission ORDER is not load-bearing because besu reads trie nodes
// by direct key lookup, not iteration order).
//
// Drift here directly threatens the cross-client genesis-root CI gate.
func TestStreamingStorageBuilder_ParityWithNonStreaming(t *testing.T) {
	cases := []struct {
		name  string
		slots []slotKV
	}{
		{
			name: "one slot",
			slots: []slotKV{
				{key: common.HexToHash("0x01"), value: common.HexToHash("0xa")},
			},
		},
		{
			name: "two slots no common prefix",
			slots: []slotKV{
				{key: common.HexToHash("0x0100000000000000000000000000000000000000000000000000000000000000"), value: common.HexToHash("0xa")},
				{key: common.HexToHash("0xff00000000000000000000000000000000000000000000000000000000000000"), value: common.HexToHash("0xb")},
			},
		},
		{
			name: "two slots long common prefix",
			slots: []slotKV{
				{key: common.HexToHash("0xabcdef0000000000000000000000000000000000000000000000000000000000"), value: common.HexToHash("0xa")},
				{key: common.HexToHash("0xabcdef0100000000000000000000000000000000000000000000000000000000"), value: common.HexToHash("0xb")},
			},
		},
		{
			name:  "ten slots sequential",
			slots: hashedSlotsRange(1, 10),
		},
		{
			name:  "one hundred random slots",
			slots: randomSlots(t, 100, 0xc0ffee),
		},
		{
			name:  "one thousand random slots",
			slots: randomSlots(t, 1000, 0xfeedface),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sort slots by keccak(slotKey) — both builders require sorted input.
			sorted := append([]slotKV(nil), tc.slots...)
			sort.Slice(sorted, func(i, j int) bool {
				hi := crypto.Keccak256Hash(sorted[i].key[:])
				hj := crypto.Keccak256Hash(sorted[j].key[:])
				return bytes.Compare(hi[:], hj[:]) < 0
			})

			oldRoot, oldEmissions := runNonStreaming(t, sorted)
			newRoot, newEmissions := runStreaming(t, sorted)

			if oldRoot != newRoot {
				t.Errorf("root mismatch:\n streaming  %s\n nonstream  %s", newRoot.Hex(), oldRoot.Hex())
			}
			if !sameEmissionMultiset(oldEmissions, newEmissions) {
				t.Errorf("emission multiset mismatch: old=%d entries, new=%d entries", len(oldEmissions), len(newEmissions))
				logEmissionDiff(t, oldEmissions, newEmissions)
			}
		})
	}
}

// TestStreamingStorageBuilder_MemoryProfile is a smoke test that the
// streaming builder doesn't OOM on a large input. Inserts 100 000 slots
// and asserts the build completes; on a passing run the resident set
// stays in the low MiB range (vs hundreds of GiB for the non-streaming
// builder at this scale).
func TestStreamingStorageBuilder_MemoryProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory smoke test in -short")
	}
	slots := randomSlots(t, 100_000, 0xdeadbeef)
	sort.Slice(slots, func(i, j int) bool {
		hi := crypto.Keccak256Hash(slots[i].key[:])
		hj := crypto.Keccak256Hash(slots[j].key[:])
		return bytes.Compare(hi[:], hj[:]) < 0
	})

	sink := &recordingSink{}
	b := New(sink)
	sb := b.BeginStreamingStorage(common.HexToHash("0xa"))
	for _, s := range slots {
		hashed := crypto.Keccak256Hash(s.key[:])
		valueRLP := besurlp.EncodeStorageValue(s.value)
		if err := sb.AddSlot(hashed, valueRLP); err != nil {
			t.Fatalf("AddSlot: %v", err)
		}
	}
	root, err := sb.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if root == besu.EmptyTrieNodeHash || (root == common.Hash{}) {
		t.Errorf("root looks empty: %s", root.Hex())
	}
}

// --- helpers ---

type slotKV struct {
	key, value common.Hash
}

// runNonStreaming feeds the given (already-sorted) slots through the
// existing in-memory StorageBuilder and returns its root + emission set.
func runNonStreaming(t *testing.T, slots []slotKV) (common.Hash, []sinkStorageRecord) {
	t.Helper()
	sink := &recordingSink{}
	b := New(sink)
	addr := common.HexToHash("0xa")
	sb := b.BeginStorage(addr)
	for _, s := range slots {
		hashed := crypto.Keccak256Hash(s.key[:])
		valueRLP := besurlp.EncodeStorageValue(s.value)
		if err := sb.AddSlot(hashed, valueRLP); err != nil {
			t.Fatalf("non-streaming AddSlot: %v", err)
		}
	}
	root, err := sb.Commit()
	if err != nil {
		t.Fatalf("non-streaming Commit: %v", err)
	}
	return root, sink.storageNodes
}

// runStreaming feeds the given (already-sorted) slots through the new
// StreamingStorageBuilder and returns its root + emission set.
func runStreaming(t *testing.T, slots []slotKV) (common.Hash, []sinkStorageRecord) {
	t.Helper()
	sink := &recordingSink{}
	b := New(sink)
	addr := common.HexToHash("0xa")
	sb := b.BeginStreamingStorage(addr)
	for _, s := range slots {
		hashed := crypto.Keccak256Hash(s.key[:])
		valueRLP := besurlp.EncodeStorageValue(s.value)
		if err := sb.AddSlot(hashed, valueRLP); err != nil {
			t.Fatalf("streaming AddSlot: %v", err)
		}
	}
	root, err := sb.Commit()
	if err != nil {
		t.Fatalf("streaming Commit: %v", err)
	}
	return root, sink.storageNodes
}

// sameEmissionMultiset compares two emission slices ignoring order.
// Sorts both by location bytes (lex) then compares element-wise.
func sameEmissionMultiset(a, b []sinkStorageRecord) bool {
	if len(a) != len(b) {
		return false
	}
	sortFn := func(s []sinkStorageRecord) {
		sort.Slice(s, func(i, j int) bool {
			if c := bytes.Compare(s[i].location, s[j].location); c != 0 {
				return c < 0
			}
			return bytes.Compare(s[i].rlp, s[j].rlp) < 0
		})
	}
	aa := append([]sinkStorageRecord(nil), a...)
	bb := append([]sinkStorageRecord(nil), b...)
	sortFn(aa)
	sortFn(bb)
	for i := range aa {
		if aa[i].addrHash != bb[i].addrHash ||
			!bytes.Equal(aa[i].location, bb[i].location) ||
			aa[i].hash != bb[i].hash ||
			!bytes.Equal(aa[i].rlp, bb[i].rlp) {
			return false
		}
	}
	return true
}

// logEmissionDiff prints a brief diff to help debug parity failures.
func logEmissionDiff(t *testing.T, a, b []sinkStorageRecord) {
	t.Helper()
	t.Logf("non-streaming emissions: %d", len(a))
	t.Logf("streaming     emissions: %d", len(b))
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	for i := 0; i < max && i < 8; i++ {
		var av, bv string
		if i < len(a) {
			av = string(a[i].location)
		}
		if i < len(b) {
			bv = string(b[i].location)
		}
		t.Logf("  [%d] old.loc=%x new.loc=%x", i, av, bv)
	}
}

// randomSlots generates n deterministic (key, value) pairs.
func randomSlots(t *testing.T, n int, seed uint64) []slotKV {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	out := make([]slotKV, n)
	for i := range out {
		var k, v common.Hash
		for j := range k {
			k[j] = byte(rng.UintN(256))
			v[j] = byte(rng.UintN(256))
		}
		out[i] = slotKV{key: k, value: v}
	}
	return out
}

// hashedSlotsRange produces N slots with sequential keys [start..start+n-1]
// (small integers), useful for deterministic test fixtures.
func hashedSlotsRange(start, n int) []slotKV {
	out := make([]slotKV, n)
	for i := 0; i < n; i++ {
		k := common.BigToHash(common.Big1)
		k[31] = byte(start + i)
		v := common.BigToHash(common.Big1)
		v[31] = byte(i + 1)
		out[i] = slotKV{key: k, value: v}
	}
	return out
}
