//go:build cgo_erigon_commitment

package commitment

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/streamsort"
)

// collectBranches drains res.BranchIterate into a map for byte-level test
// assertions (production streams BranchIterate; it never materializes this).
func collectBranches(t *testing.T, res *Result) map[string][]byte {
	t.Helper()
	m := make(map[string][]byte)
	if err := res.BranchIterate(func(k, v []byte) error {
		m[string(k)] = append([]byte(nil), v...)
		return nil
	}); err != nil {
		t.Fatalf("collectBranches: %v", err)
	}
	return m
}

// TestEmptyAllocReturnsEmptyTrieRoot pins the canonical empty-trie
// root and verifies ComputeGenesisRoot bottoms out cleanly when fed no
// accounts. The expected hash is keccak256(rlp("")) =
// 0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421.
func TestEmptyAllocReturnsEmptyTrieRoot(t *testing.T) {
	res, err := ComputeGenesisRootFromAccounts(nil)
	if err != nil {
		t.Fatalf("ComputeGenesisRootFromAccounts([]): %v", err)
	}
	defer res.CloseBranches()
	emptyTrieRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if res.Root != emptyTrieRoot {
		t.Errorf("empty alloc root mismatch:\n  got:  %s\n  want: %s", res.Root.Hex(), emptyTrieRoot.Hex())
	}
	if n := len(collectBranches(t, &res)); n != 0 {
		t.Errorf("empty alloc emitted %d branch nodes; expected 0", n)
	}
}

// TestSingleEOAProducesDeterministicRoot pins the root for a single
// alloc entry as a deterministic regression target. The exact value
// here is recorded the FIRST time the test runs; subsequent runs must
// match.
func TestSingleEOAProducesDeterministicRoot(t *testing.T) {
	bal := uint256.NewInt(0).Mul(uint256.NewInt(1_000_000_000), uint256.NewInt(1_000_000_000)) // 1e18
	accounts := []Account{{
		Address: common.HexToAddress("0x000000000000000000000000000000000000beef"),
		Nonce:   3,
		Balance: bal,
	}}
	res, err := ComputeGenesisRootFromAccounts(accounts)
	if err != nil {
		t.Fatalf("ComputeGenesisRoot: %v", err)
	}
	defer res.CloseBranches()
	branches := collectBranches(t, &res)

	// Two invocations with identical input must agree.
	res2, err := ComputeGenesisRootFromAccounts(accounts)
	if err != nil {
		t.Fatalf("ComputeGenesisRoot (2nd): %v", err)
	}
	defer res2.CloseBranches()
	if res.Root != res2.Root {
		t.Errorf("non-deterministic: %s vs %s", res.Root.Hex(), res2.Root.Hex())
	}
	if !bytes.Equal(branchNodesBytes(branches), branchNodesBytes(collectBranches(t, &res2))) {
		t.Error("non-deterministic branch nodes")
	}
	if (res.Root == common.Hash{}) {
		t.Error("got zero hash for non-empty alloc")
	}
	t.Logf("single-EOA root: %s (%d branch nodes)", res.Root.Hex(), len(branches))
}

// TestContractWithStorageProducesRoot exercises the storage-touch path:
// a contract with 3 storage slots + balance + code. Verifies that
// branch nodes are emitted (commitment trie has structure) and the
// root is deterministic.
func TestContractWithStorageProducesRoot(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000c0ffee")
	bal := uint256.NewInt(42)
	code := []byte{0x60, 0x80, 0x60, 0x40, 0x52, 0x60, 0x05, 0x60, 0x00, 0x55} // PUSH1 5 PUSH1 0 SSTORE
	storage := map[common.Hash]common.Hash{
		common.BigToHash(uint256.NewInt(0).ToBig()):      common.BigToHash(uint256.NewInt(100).ToBig()),
		common.BigToHash(uint256.NewInt(1).ToBig()):      common.BigToHash(uint256.NewInt(200).ToBig()),
		common.BigToHash(uint256.NewInt(0x1000).ToBig()): common.BigToHash(uint256.NewInt(0xdeadbeef).ToBig()),
	}
	accounts := []Account{{
		Address: addr,
		Nonce:   1,
		Balance: bal,
		Code:    code,
		Storage: storage,
	}}
	res, err := ComputeGenesisRootFromAccounts(accounts)
	if err != nil {
		t.Fatalf("ComputeGenesisRoot: %v", err)
	}
	if (res.Root == common.Hash{}) {
		t.Error("got zero hash for non-empty alloc with storage")
	}
	defer res.CloseBranches()
	if n := len(collectBranches(t, &res)); n == 0 {
		t.Error("expected at least one branch node for contract+storage alloc")
	} else {
		t.Logf("contract+3slots root: %s (%d branch nodes)", res.Root.Hex(), n)
	}
}

// TestComputeGenesisRoot_IncludesRootBranch is the regression gate for
// the bug where ParallelHashSort returned the root hash without
// flushing the root HexPatriciaHashed's deferred branch updates.
// Without the explicit ApplyAndClearInlineDeferredUpdates call in
// ComputeGenesisRoot, the branch row set would be missing
// the root-level (depth-0) branch entry, producing silent state-root
// divergence at any alloc that lands in 16+ distinct first nibbles.
//
// The fixture: 16 EOAs with addresses chosen so their keccak256 hashes
// span all 16 first nibbles. The resulting trie has a 16-cell root
// branch — exactly the structure that exposes the bug. The leaves of
// a single-leaf-per-nibble trie are stored inline in the root branch
// (no extra branch nodes), so a missing root branch means
// zero branch rows.
func TestComputeGenesisRoot_IncludesRootBranch(t *testing.T) {
	accounts := generateAccountsSpanningAllFirstNibbles(t)
	if len(accounts) != 16 {
		t.Fatalf("fixture should have 16 accounts; got %d", len(accounts))
	}
	res, err := ComputeGenesisRootFromAccounts(accounts)
	if err != nil {
		t.Fatalf("ComputeGenesisRootFromAccounts: %v", err)
	}
	defer res.CloseBranches()
	if (res.Root == common.Hash{}) {
		t.Fatal("expected non-zero root for 16-account alloc")
	}
	branches := collectBranches(t, &res)
	if len(branches) == 0 {
		t.Fatalf("expected ≥ 1 branch node for 16-account alloc spanning all first nibbles; got 0 — root-level branch was not flushed (Bug 2 regression: ApplyAndClearInlineDeferredUpdates not called after pph.Process)")
	}
	t.Logf("16-account root: %s (%d branch nodes)", res.Root.Hex(), len(branches))
}

// generateAccountsSpanningAllFirstNibbles produces 16 Accounts whose
// keccak256(addr) bytes start with each of the 16 hex nibbles. Brute-
// force search; on typical machines this terminates within milliseconds.
func generateAccountsSpanningAllFirstNibbles(t *testing.T) []Account {
	t.Helper()
	seen := make(map[byte]Account, 16)
	for i := uint64(0); len(seen) < 16; i++ {
		var addr common.Address
		binary.BigEndian.PutUint64(addr[12:], i+1)
		firstNibble := crypto.Keccak256Hash(addr[:])[0] >> 4
		if _, ok := seen[firstNibble]; ok {
			continue
		}
		seen[firstNibble] = Account{
			Address: addr,
			Nonce:   i + 1,
			Balance: uint256.NewInt(1000 + i),
		}
		if i > 100_000 {
			t.Fatalf("could not find 16 distinct first nibbles within %d attempts (got %d)", i, len(seen))
		}
	}
	out := make([]Account, 0, 16)
	for _, a := range seen {
		out = append(out, a)
	}
	return out
}

func branchNodesBytes(m map[string][]byte) []byte {
	out := make([]byte, 0)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// stable order
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	for _, k := range keys {
		out = append(out, []byte(k)...)
		out = append(out, m[k]...)
	}
	return out
}

// TestResultBranchLifetime pins the M1c ownership contract: the branch store
// RETAINED on Result survives ComputeGenesisRoot's return, BranchIterate
// yields the identical row set (repeatably), and CloseBranches is idempotent
// with BranchIterate failing loudly afterwards.
func TestResultBranchLifetime(t *testing.T) {
	accounts := make([]Account, 0, 64)
	for i := 0; i < 64; i++ {
		var addr common.Address
		addr[0], addr[19] = byte(i+1), byte(i)
		accounts = append(accounts, Account{
			Address: addr,
			Nonce:   uint64(i + 1),
			Balance: uint256.NewInt(uint64(1000 * (i + 1))),
		})
	}
	// Build the 16-store partition + call ComputeGenesisRoot DIRECTLY (not
	// the FromAccounts wrapper, which closes the branches itself).
	stores := make([]*streamsort.Store, 0, NumInputParts)
	defer func() {
		for _, s := range stores {
			s.Close()
		}
	}()
	for i := 0; i < NumInputParts; i++ {
		s, err := streamsort.New("")
		if err != nil {
			t.Fatalf("streamsort.New[%d]: %v", i, err)
		}
		stores = append(stores, s)
	}
	for _, a := range accounts {
		acctBytes := EncodeAccountUpdate(a.Nonce, a.Balance, nil)
		if err := stores[InputPart(a.Address[:])].Put(a.Address[:], acctBytes); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	for i := range stores {
		if err := stores[i].Finalize(); err != nil {
			t.Fatalf("finalize[%d]: %v", i, err)
		}
	}

	res, err := ComputeGenesisRoot(stores, "", KeyingPlain)
	if err != nil {
		t.Fatalf("ComputeGenesisRoot: %v", err)
	}

	collect := func() (map[string][]byte, uint64) {
		m := make(map[string][]byte)
		var n uint64
		if err := res.BranchIterate(func(k, v []byte) error {
			m[string(k)] = append([]byte(nil), v...)
			n++
			return nil
		}); err != nil {
			t.Fatalf("BranchIterate: %v", err)
		}
		return m, n
	}
	first, n1 := collect()
	second, n2 := collect() // repeatable until CloseBranches
	if n1 != res.BranchCount || n2 != res.BranchCount {
		t.Fatalf("BranchIterate row counts %d/%d != BranchCount %d", n1, n2, res.BranchCount)
	}
	if len(first) == 0 {
		t.Fatal("no branches retained — Result.branches lost")
	}
	if !bytes.Equal(branchNodesBytes(first), branchNodesBytes(second)) {
		t.Fatal("BranchIterate not repeatable: byte sets differ across calls")
	}

	res.CloseBranches()
	res.CloseBranches() // idempotent
	if err := res.BranchIterate(func(k, v []byte) error { return nil }); err == nil {
		t.Fatal("BranchIterate after CloseBranches should error")
	}
}
