package nimbus

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/ethrex"
)

// builder_reference_test.go pins the Builder against go-ethereum's StackTrie
// (an independent MPT implementation) for the root hash, and against
// VerifyState for the vertex layout nimbus will read.

type testSlot struct {
	keyHash common.Hash // keccak(slot) — the trie path
	value   common.Hash
}

type testAccount struct {
	addrHash common.Hash
	nonce    uint64
	balance  *uint256.Int
	codeHash common.Hash
	slots    []testSlot
}

type builtState struct {
	rows    VertexRows
	admin   SavedState
	root    common.Hash
	refRoot common.Hash
	stoIDs  map[common.Hash]VertexID
	hints   map[common.Hash]byte
}

func collectSink(t *testing.T, rows VertexRows) Sink {
	return func(rvid RootedVertexID, rec []byte) error {
		if _, dup := rows[rvid]; dup {
			return fmt.Errorf("duplicate row %+v", rvid)
		}
		rows[rvid] = append([]byte(nil), rec...)
		return nil
	}
}

func stackTrieRoot(t *testing.T, keys []common.Hash, values [][]byte) common.Hash {
	t.Helper()
	st := trie.NewStackTrie(nil)
	for i, k := range keys {
		if err := st.Update(k[:], values[i]); err != nil {
			t.Fatalf("StackTrie.Update: %v", err)
		}
	}
	return st.Hash()
}

// buildState writes every storage trie then the account trie through the
// Builder, exactly as client/nimbus does, and computes the reference roots.
func buildState(t *testing.T, accounts []testAccount) builtState {
	t.Helper()
	sort.Slice(accounts, func(i, j int) bool { return bytes.Compare(accounts[i].addrHash[:], accounts[j].addrHash[:]) < 0 })
	alloc := NewVidAllocator()
	rows := VertexRows{}
	sink := collectSink(t, rows)
	bs := builtState{rows: rows, stoIDs: map[common.Hash]VertexID{}, hints: map[common.Hash]byte{}}

	var acctKeys []common.Hash
	var acctVals [][]byte
	ab := NewBuilder(StateRootVID, alloc, sink)
	for _, a := range accounts {
		storageRoot := EmptyRootHash
		var stoID VertexID
		var hint byte
		slots := append([]testSlot(nil), a.slots...)
		sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i].keyHash[:], slots[j].keyHash[:]) < 0 })
		if len(slots) > 0 {
			stoID = alloc.Alloc(1)
			sb := NewBuilder(stoID, alloc, sink)
			var keys []common.Hash
			var vals [][]byte
			for _, s := range slots {
				valueRLP := ethrex.EncodeStorageValueBytes32(s.value)
				if err := sb.AddLeaf(ethrex.BytesToNibbles(s.keyHash[:]), valueRLP, AppendStoLeafPayload(nil, s.value)); err != nil {
					t.Fatalf("storage AddLeaf: %v", err)
				}
				keys = append(keys, s.keyHash)
				vals = append(vals, valueRLP)
			}
			root, err := sb.Root()
			if err != nil {
				t.Fatalf("storage Root: %v", err)
			}
			if ref := stackTrieRoot(t, keys, vals); ref != root {
				t.Fatalf("storage root %s != StackTrie %s (%d slots)", root.Hex(), ref.Hex(), len(slots))
			}
			storageRoot = root
			hint = sb.StoHint()
			bs.stoIDs[a.addrHash] = stoID
			bs.hints[a.addrHash] = hint
		}
		valueRLP := ethrex.EncodeAccountState(a.nonce, a.balance, storageRoot, a.codeHash)
		payload := AppendAccLeafPayload(nil, a.nonce, a.balance, stoID, hint, a.codeHash)
		if err := ab.AddLeaf(ethrex.BytesToNibbles(a.addrHash[:]), valueRLP, payload); err != nil {
			t.Fatalf("account AddLeaf: %v", err)
		}
		acctKeys = append(acctKeys, a.addrHash)
		acctVals = append(acctVals, valueRLP)
	}
	root, err := ab.Root()
	if err != nil {
		t.Fatalf("account Root: %v", err)
	}
	bs.root = root
	bs.refRoot = stackTrieRoot(t, acctKeys, acctVals)
	bs.admin = SavedState{VTop: alloc.VTop()}
	return bs
}

func randomValue(rng *rand.Rand) common.Hash {
	var v common.Hash
	switch rng.Intn(4) {
	case 0:
		v[31] = byte(1 + rng.Intn(0x7f))
	case 1:
		v[31] = byte(0x80 + rng.Intn(0x80))
	default:
		rng.Read(v[:])
		for z := rng.Intn(32); z > 0; z-- {
			v[z-1] = 0
		}
		if v == (common.Hash{}) {
			v[31] = 1
		}
	}
	return v
}

func randomHash(rng *rand.Rand) common.Hash {
	var h common.Hash
	rng.Read(h[:])
	return h
}

func randomAccounts(rng *rand.Rand, n int) []testAccount {
	seen := map[common.Hash]bool{}
	out := make([]testAccount, 0, n)
	for len(out) < n {
		a := testAccount{addrHash: randomHash(rng), balance: new(uint256.Int), codeHash: EmptyCodeHash}
		if seen[a.addrHash] {
			continue
		}
		seen[a.addrHash] = true
		if rng.Intn(4) != 0 {
			a.nonce = rng.Uint64() >> uint(rng.Intn(64))
		}
		if rng.Intn(10) != 0 {
			var b [32]byte
			rng.Read(b[:])
			a.balance.SetBytes(b[rng.Intn(32):])
		}
		if rng.Intn(5) < 2 { // contract
			rng.Read(a.codeHash[:])
			count := []int{0, 1, 2, 3, 10, 50, 300}[rng.Intn(7)]
			slotSeen := map[common.Hash]bool{}
			for len(a.slots) < count {
				s := testSlot{keyHash: randomHash(rng), value: randomValue(rng)}
				if slotSeen[s.keyHash] {
					continue
				}
				slotSeen[s.keyHash] = true
				a.slots = append(a.slots, s)
			}
		}
		out = append(out, a)
	}
	return out
}

func verifyBuilt(t *testing.T, bs builtState) *VerifyReport {
	t.Helper()
	if bs.root != bs.refRoot {
		t.Fatalf("account root %s != StackTrie %s", bs.root.Hex(), bs.refRoot.Hex())
	}
	rep, err := VerifyState(bs.rows, bs.admin, bs.root)
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	return rep
}

func TestBuilderRandomStates(t *testing.T) {
	for _, n := range []int{1, 2, 3, 17, 300, 2000} {
		seeds := 20
		if n >= 300 {
			seeds = 3
		}
		for seed := 0; seed < seeds; seed++ {
			rng := rand.New(rand.NewSource(int64(n*1000 + seed)))
			accounts := randomAccounts(rng, n)
			bs := buildState(t, accounts)
			rep := verifyBuilt(t, bs)
			if rep.Accounts != n {
				t.Fatalf("n=%d seed=%d: report %+v", n, seed, rep)
			}
			if rep.StorageTries != len(bs.stoIDs) || rep.DynamicVertices < rep.StorageTries {
				t.Fatalf("n=%d seed=%d: report %+v vs %d storage tries", n, seed, rep, len(bs.stoIDs))
			}
			for addr, hint := range bs.hints {
				if hint < 1 || hint > MaxStoHint {
					t.Fatalf("account %x hint %d", addr, hint)
				}
			}
		}
	}
}

// withPrefix returns a random 32-byte hash whose first n nibbles equal prefix.
func withPrefix(rng *rand.Rand, prefix []byte) common.Hash {
	h := randomHash(rng)
	nib := ethrex.BytesToNibbles(h[:])
	copy(nib, prefix)
	var out common.Hash
	for i := 0; i < 32; i++ {
		out[i] = nib[2*i]<<4 | nib[2*i+1]
	}
	return out
}

// Deep shared prefixes force branches below StaticVIDLevels (dynamic
// 16-blocks, ExtBranch records under the static range) in both tries.
func TestBuilderDeepSharedPrefix(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	prefix := ethrex.BytesToNibbles(randomHash(rng).Bytes())[:11]
	var accounts []testAccount
	for i := 0; i < 40; i++ {
		a := testAccount{addrHash: withPrefix(rng, prefix[:9+rng.Intn(3)]), balance: uint256.NewInt(uint64(i + 1)), codeHash: EmptyCodeHash}
		if i%3 == 0 {
			rng.Read(a.codeHash[:])
			sp := ethrex.BytesToNibbles(randomHash(rng).Bytes())[:10]
			for j := 0; j < 30; j++ {
				a.slots = append(a.slots, testSlot{keyHash: withPrefix(rng, sp), value: randomValue(rng)})
			}
		}
		accounts = append(accounts, a)
	}
	accounts = append(accounts, randomAccounts(rng, 20)...)
	bs := buildState(t, accounts)
	rep := verifyBuilt(t, bs)
	if rep.DynamicVertices <= rep.StorageTries {
		t.Fatalf("expected deep dynamic vertices beyond the storage roots: %+v", rep)
	}
	deepAccountRows := 0
	for rvid := range bs.rows {
		if rvid.Root == StateRootVID && rvid.Vid >= FirstDynamicVID {
			deepAccountRows++
		}
	}
	if deepAccountRows == 0 {
		t.Fatal("no dynamic-ID rows in the account trie")
	}
}

// A plain Branch whose children are short leaves has < 32-byte node RLP, so
// its parent embeds it inline and the record stores no key. Shape: a 59-nibble
// shared prefix P, keys P‖0‖0‖*, P‖0‖1‖*, P‖1‖* — the root ExtBranch (pfx P)
// splits at nibble 59 and its child 0 is a keyless Branch splitting at 60.
func TestBuilderInlineChildren(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	sp := ethrex.BytesToNibbles(randomHash(rng).Bytes())[:59]
	a := testAccount{addrHash: randomHash(rng), balance: uint256.NewInt(1), codeHash: EmptyCodeHash}
	for j, tail := range [][]byte{{0, 0}, {0, 1}, {1}} {
		var v common.Hash
		v[31] = byte(j + 1)
		a.slots = append(a.slots, testSlot{keyHash: withPrefix(rng, append(append([]byte(nil), sp...), tail...)), value: v})
	}
	bs := buildState(t, []testAccount{a, {addrHash: randomHash(rng), balance: uint256.NewInt(2), codeHash: EmptyCodeHash}})
	verifyBuilt(t, bs)
	stoID := bs.stoIDs[a.addrHash]
	keyless, keyed := 0, 0
	for rvid, rec := range bs.rows {
		if rvid.Root != stoID {
			continue
		}
		v, err := DecodeVertex(rec)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case v.Type == Branch && v.Key == nil:
			keyless++
		case v.Type == ExtBranch && v.Key != nil:
			keyed++
		}
	}
	if keyless != 1 || keyed != 1 {
		t.Fatalf("expected one keyless Branch and one keyed ExtBranch, found %d / %d", keyless, keyed)
	}
}

func TestBuilderLastNibbleDiff(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	base := randomHash(rng)
	other := base
	other[31] ^= 0x01
	a := testAccount{addrHash: randomHash(rng), balance: uint256.NewInt(1), codeHash: EmptyCodeHash,
		slots: []testSlot{{base, common.HexToHash("0x01")}, {other, common.HexToHash("0x02")}}}
	bs := buildState(t, []testAccount{a, {addrHash: randomHash(rng), balance: uint256.NewInt(2), codeHash: EmptyCodeHash}})
	verifyBuilt(t, bs)
	emptyPfx := 0
	for rvid, rec := range bs.rows {
		if rvid.Root == bs.stoIDs[a.addrHash] && bytes.HasSuffix(rec, []byte{0x20, 0x41}) {
			emptyPfx++
		}
	}
	if emptyPfx != 2 {
		t.Fatalf("expected two empty-prefix leaves, found %d", emptyPfx)
	}
	if bs.hints[a.addrHash] != MaxStoHint {
		t.Fatalf("leaves at position 63 should hint %d, got %d", MaxStoHint, bs.hints[a.addrHash])
	}
}

func TestBuilderSingleLeafTries(t *testing.T) {
	rng := rand.New(rand.NewSource(14))
	a := testAccount{addrHash: randomHash(rng), nonce: 5, balance: uint256.NewInt(7), codeHash: EmptyCodeHash,
		slots: []testSlot{{randomHash(rng), common.HexToHash("0x0102")}}}
	bs := buildState(t, []testAccount{a})
	rep := verifyBuilt(t, bs)
	if len(bs.rows) != 2 || rep.StorageLeaves != 1 || rep.Branches+rep.ExtBranches != 0 {
		t.Fatalf("rows %d report %+v", len(bs.rows), rep)
	}
	if bs.hints[a.addrHash] != 1 {
		t.Fatalf("root-leaf storage trie should hint 1, got %d", bs.hints[a.addrHash])
	}
	if bs.admin.VTop != uint64(FirstDynamicVID) {
		t.Fatalf("vTop %d", bs.admin.VTop)
	}
	leaf, err := DecodeVertex(bs.rows[RootedVertexID{StateRootVID, StateRootVID}])
	if err != nil || leaf.Type != AccLeaf || len(leaf.Pfx) != 64 || leaf.StoID != FirstDynamicVID {
		t.Fatalf("root leaf %+v %v", leaf, err)
	}
}

func TestBuilderEmptyAndOrder(t *testing.T) {
	alloc := NewVidAllocator()
	rows := VertexRows{}
	b := NewBuilder(StateRootVID, alloc, collectSink(t, rows))
	root, err := b.Root()
	if err != nil || root != EmptyRootHash || len(rows) != 0 || b.StoHint() != 0 {
		t.Fatalf("empty trie: %s %v rows=%d", root.Hex(), err, len(rows))
	}
	b = NewBuilder(StateRootVID, alloc, nil)
	k1 := bytes.Repeat([]byte{2}, 64)
	k0 := bytes.Repeat([]byte{1}, 64)
	if err := b.AddLeaf(k1, []byte{0x01}, AppendStoLeafPayload(nil, common.HexToHash("0x01"))); err != nil {
		t.Fatal(err)
	}
	if err := b.AddLeaf(k0, []byte{0x01}, nil); err != ErrKeysOutOfOrder {
		t.Fatalf("out-of-order insert returned %v", err)
	}
	if err := b.AddLeaf(k1, []byte{0x01}, nil); err != ErrKeysOutOfOrder {
		t.Fatalf("duplicate insert returned %v", err)
	}
	if alloc.VTop() != 0 {
		t.Fatalf("static-only builds must not allocate: vTop %d", alloc.VTop())
	}
}

func TestStreamHashBuilderMatchesBuilder(t *testing.T) {
	rng := rand.New(rand.NewSource(15))
	var slots []testSlot
	for i := 0; i < 200; i++ {
		slots = append(slots, testSlot{randomHash(rng), randomValue(rng)})
	}
	sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i].keyHash[:], slots[j].keyHash[:]) < 0 })

	rowsA, rowsB := VertexRows{}, VertexRows{}
	ba := NewBuilder(FirstDynamicVID, NewVidAllocator(), collectSink(t, rowsA))
	sb := NewStreamHashBuilder(FirstDynamicVID, NewVidAllocator(), collectSink(t, rowsB))
	for _, s := range slots {
		rlp := ethrex.EncodeStorageValueBytes32(s.value)
		if err := ba.AddLeaf(ethrex.BytesToNibbles(s.keyHash[:]), rlp, AppendStoLeafPayload(nil, s.value)); err != nil {
			t.Fatal(err)
		}
		if err := sb.AddLeaf(s.keyHash, rlp); err != nil {
			t.Fatal(err)
		}
	}
	ra, _ := ba.Root()
	rb, _ := sb.Root()
	if ra != rb || ba.StoHint() != sb.StoHint() || sb.LeafCount() != 200 {
		t.Fatalf("roots %s/%s hints %d/%d", ra.Hex(), rb.Hex(), ba.StoHint(), sb.StoHint())
	}
	if len(rowsA) != len(rowsB) {
		t.Fatalf("row counts %d/%d", len(rowsA), len(rowsB))
	}
	for k, v := range rowsA {
		if !bytes.Equal(rowsB[k], v) {
			t.Fatalf("row %+v differs", k)
		}
	}
	if crypto.Keccak256Hash(nil) == ra {
		t.Fatal("unexpected root")
	}
}
