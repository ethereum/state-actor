package reth

import (
	"errors"
	"math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/entitygen"
)

func TestComputeStateRootEmpty(t *testing.T) {
	got, err := ComputeStateRoot(nil, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot(nil, nil): %v", err)
	}
	want := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if got != want {
		t.Errorf("empty root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestComputeStateRootSingleEOA(t *testing.T) {
	acc := makeTestEOA(t, common.HexToAddress("0xaabb"), 100, 1)
	got, err := ComputeStateRoot([]*entitygen.Account{acc}, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}

	// Cross-check against go-ethereum's StackTrie.
	st := trie.NewStackTrie(nil)
	rlpBytes := mustRLP(t, acc.StateAccount)
	if err := st.Update(acc.AddrHash[:], rlpBytes); err != nil {
		t.Fatalf("StackTrie.Update: %v", err)
	}
	want := st.Hash()

	if got != want {
		t.Errorf("single-EOA root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestComputeStateRootManyEOAs(t *testing.T) {
	rng := rand.New(rand.NewSource(0xd00d))
	const n = 50
	accounts := make([]*entitygen.Account, n)
	for i := 0; i < n; i++ {
		accounts[i] = entitygen.GenerateEOA(rng)
	}

	got, err := ComputeStateRoot(accounts, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}

	// Reference: feed sorted-by-AddrHash directly into StackTrie.
	sorted := make([]*entitygen.Account, n)
	copy(sorted, accounts)
	sortAccountsByAddrHash(sorted)

	st := trie.NewStackTrie(nil)
	for _, a := range sorted {
		rlpBytes := mustRLP(t, a.StateAccount)
		if err := st.Update(a.AddrHash[:], rlpBytes); err != nil {
			t.Fatalf("StackTrie.Update: %v", err)
		}
	}
	want := st.Hash()

	if got != want {
		t.Errorf("50-EOA root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

// makeTestEOA creates a deterministic test EOA with the given address,
// balance in ETH, and nonce. Root and CodeHash are set to the empty values
// matching what GenerateEOA produces.
func makeTestEOA(t *testing.T, addr common.Address, balanceETH uint64, nonce uint64) *entitygen.Account {
	t.Helper()
	bal := new(uint256.Int).Mul(uint256.NewInt(balanceETH), uint256.NewInt(1e18))
	return &entitygen.Account{
		Address:  addr,
		AddrHash: crypto.Keccak256Hash(addr[:]),
		StateAccount: &types.StateAccount{
			Nonce:    nonce,
			Balance:  bal,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
	}
}

// mustRLP encodes v using go-ethereum's RLP encoder or fails the test.
func mustRLP(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := rlp.EncodeToBytes(v)
	if err != nil {
		t.Fatalf("rlp.EncodeToBytes: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Storage root tests — cross-validate computeStorageRoot against go-ethereum's
// StateTrie.UpdateStorage path (i.e., StackTrie with keccak-hashed keys and
// trimmed-zero RLP values).
// ---------------------------------------------------------------------------

// storageRootViaStackTrie computes the storage trie root using go-ethereum's
// StackTrie, exactly mirroring StateTrie.UpdateStorage:
//
//	key = keccak256(raw_slot_key)
//	value = rlp.EncodeToBytes(TrimLeftZeroes(raw_32_byte_value))
//
// Slots are sorted by hashed key before insertion (StackTrie requires
// ascending key order). Zero-valued slots are skipped (they represent
// deletions in the trie).
func storageRootViaStackTrie(t *testing.T, slots []entitygen.StorageSlot) common.Hash {
	t.Helper()

	type hashedSlot struct {
		keyHash [32]byte
		valRLP  []byte
	}
	sorted := make([]hashedSlot, 0, len(slots))
	for _, s := range slots {
		if s.Value == (common.Hash{}) {
			continue // zero value = deletion, skip
		}
		kh := crypto.Keccak256(s.Key[:])
		trimmed := common.TrimLeftZeroes(s.Value[:])
		valRLP, err := rlp.EncodeToBytes(trimmed)
		if err != nil {
			t.Fatalf("storageRootViaStackTrie: rlp.EncodeToBytes: %v", err)
		}
		var h [32]byte
		copy(h[:], kh)
		sorted = append(sorted, hashedSlot{keyHash: h, valRLP: valRLP})
	}
	if len(sorted) == 0 {
		return types.EmptyRootHash
	}
	sort.Slice(sorted, func(i, j int) bool {
		return string(sorted[i].keyHash[:]) < string(sorted[j].keyHash[:])
	})
	st := trie.NewStackTrie(nil)
	for _, s := range sorted {
		if err := st.Update(s.keyHash[:], s.valRLP); err != nil {
			t.Fatalf("storageRootViaStackTrie: StackTrie.Update: %v", err)
		}
	}
	return st.Hash()
}

// TestComputeStorageRootSingleSlot checks a single non-zero slot.
func TestComputeStorageRootSingleSlot(t *testing.T) {
	key := common.HexToHash("0xdeadbeef00000000000000000000000000000000000000000000000000000000")
	val := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	slots := []entitygen.StorageSlot{{Key: key, Value: val}}

	got, err := computeStorageRoot(slots, nil)
	if err != nil {
		t.Fatalf("computeStorageRoot: %v", err)
	}
	want := storageRootViaStackTrie(t, slots)
	if got != want {
		t.Errorf("single-slot storage root mismatch:\n  got  = %s\n  want = %s", got.Hex(), want.Hex())
	}
}

// TestComputeStorageRootMultipleSlots checks five non-zero slots.
func TestComputeStorageRootMultipleSlots(t *testing.T) {
	slots := []entitygen.StorageSlot{
		{Key: common.HexToHash("0x01"), Value: common.HexToHash("0xff")},
		{Key: common.HexToHash("0x02"), Value: common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")},
		{Key: common.HexToHash("0x03"), Value: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")},
		{Key: common.HexToHash("0x04"), Value: common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{Key: common.HexToHash("0x05"), Value: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000080")},
	}

	got, err := computeStorageRoot(slots, nil)
	if err != nil {
		t.Fatalf("computeStorageRoot: %v", err)
	}
	want := storageRootViaStackTrie(t, slots)
	if got != want {
		t.Errorf("multi-slot storage root mismatch:\n  got  = %s\n  want = %s", got.Hex(), want.Hex())
	}
}

// TestComputeStorageRootGeneratedContracts cross-validates computeStorageRoot
// against the StackTrie reference for contracts generated by entitygen, using
// the same seed / parameters as the oracle boot test.
func TestComputeStorageRootGeneratedContracts(t *testing.T) {
	const (
		seed      = int64(42)
		codeSize  = 256
		slotCount = 2
	)
	rng := rand.New(rand.NewSource(seed))
	// Advance the RNG past 10 EOAs (same order as the oracle test's RunCgo call).
	for i := 0; i < 10; i++ {
		entitygen.GenerateEOA(rng)
	}
	// Now generate 3 contracts.
	for i := 0; i < 3; i++ {
		c := entitygen.GenerateContract(rng, codeSize, slotCount)
		codeHash := crypto.Keccak256Hash(c.Code)
		c.StateAccount.CodeHash = codeHash.Bytes()

		got, err := computeStorageRoot(c.Storage, nil)
		if err != nil {
			t.Fatalf("contract %d: computeStorageRoot: %v", i, err)
		}
		want := storageRootViaStackTrie(t, c.Storage)
		if got != want {
			t.Errorf("contract %d storage root mismatch:\n  got  = %s\n  want = %s", i, got.Hex(), want.Hex())
		}
	}
}

// TestComputeStateRootWithContracts is the end-to-end test: generate contracts,
// compute storage roots, set StateAccount.Root, then verify ComputeStateRoot
// matches go-ethereum's StackTrie for the whole state trie.
func TestComputeStateRootWithContracts(t *testing.T) {
	const (
		seed       = int64(42)
		nAccounts  = 10
		nContracts = 3
		codeSize   = 256
		slotCount  = 2
	)
	rng := rand.New(rand.NewSource(seed))

	accounts := make([]*entitygen.Account, 0, nAccounts+nContracts)

	// Generate EOAs.
	for i := 0; i < nAccounts; i++ {
		accounts = append(accounts, entitygen.GenerateEOA(rng))
	}

	// Generate contracts and compute storage root inline.
	for i := 0; i < nContracts; i++ {
		c := entitygen.GenerateContract(rng, codeSize, slotCount)
		storageRoot, err := computeStorageRoot(c.Storage, nil)
		if err != nil {
			t.Fatalf("contract %d: computeStorageRoot: %v", i, err)
		}
		codeHash := crypto.Keccak256Hash(c.Code)
		c.StateAccount.Root = storageRoot
		c.StateAccount.CodeHash = codeHash.Bytes()
		accounts = append(accounts, c)
	}

	// Our implementation.
	got, err := ComputeStateRoot(accounts, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}

	// Reference: feed all accounts (sorted by AddrHash) into go-ethereum StackTrie.
	sorted := make([]*entitygen.Account, len(accounts))
	copy(sorted, accounts)
	sortAccountsByAddrHash(sorted)

	st := trie.NewStackTrie(nil)
	for _, a := range sorted {
		rlpBytes := mustRLP(t, a.StateAccount)
		if err := st.Update(a.AddrHash[:], rlpBytes); err != nil {
			t.Fatalf("StackTrie.Update %s: %v", a.Address.Hex(), err)
		}
	}
	want := st.Hash()

	if got != want {
		t.Errorf("state root with contracts mismatch:\n  got  = %s\n  want = %s", got.Hex(), want.Hex())
	}
}

// TestComputeStateRootStreaming_MatchesLegacy drives the streaming helper
// with a hand-built sorted iterator over the same accounts the legacy path
// would consume. Both must produce the same root — that's the whole point
// of the streaming variant existing.
func TestComputeStateRootStreaming_MatchesLegacy(t *testing.T) {
	const seed, n = int64(1234), 200
	rng := rand.New(rand.NewSource(seed))
	accounts := make([]*entitygen.Account, n)
	for i := range accounts {
		accounts[i] = entitygen.GenerateEOA(rng)
	}

	legacyRoot, err := ComputeStateRoot(accounts, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}

	// Pre-sort by AddrHash to honour the streaming contract.
	sorted := make([]*entitygen.Account, n)
	copy(sorted, accounts)
	sortAccountsByAddrHash(sorted)

	streamRoot, err := ComputeStateRootStreaming(func(yield func(addrHash, accountRLP []byte) error) error {
		for _, a := range sorted {
			rlpBytes := mustRLP(t, a.StateAccount)
			if err := yield(a.AddrHash[:], rlpBytes); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("ComputeStateRootStreaming: %v", err)
	}
	if legacyRoot != streamRoot {
		t.Fatalf("streaming root mismatch:\n  legacy = %s\n  stream = %s",
			legacyRoot.Hex(), streamRoot.Hex())
	}
}

// TestComputeStateRootStreaming_Empty mirrors the legacy empty-input case:
// the canonical empty-MPT root must come back when the iterator yields
// nothing.
func TestComputeStateRootStreaming_Empty(t *testing.T) {
	got, err := ComputeStateRootStreaming(func(yield func(addrHash, accountRLP []byte) error) error {
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("ComputeStateRootStreaming: %v", err)
	}
	want := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if got != want {
		t.Errorf("empty streaming root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

// TestComputeStateRootStreaming_IterErrorPropagates confirms that an error
// returned from the iter callback short-circuits and surfaces unchanged
// (wrapped) so RunCgo can attribute Pebble iterate failures to the right
// phase.
func TestComputeStateRootStreaming_IterErrorPropagates(t *testing.T) {
	sentinel := errors.New("iter sentinel")
	_, err := ComputeStateRootStreaming(func(yield func(addrHash, accountRLP []byte) error) error {
		return sentinel
	}, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want error wrapping sentinel %v", err, sentinel)
	}
}
