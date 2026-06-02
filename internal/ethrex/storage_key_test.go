package ethrex

import (
	"encoding/hex"
	"math/big"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

// ---------------------------------------------------------------------------
// StoragePrefix / PrefixedSink golden tests vs genesis_dump.json
// ---------------------------------------------------------------------------

// TestStoragePrefixGolden verifies that StoragePrefix(addrHash) produces the
// exact 66-nibble prefix that all storage_trie_nodes keys share in the dump.
// The storage-trie root row is keyed by exactly this prefix.
func TestStoragePrefixGolden(t *testing.T) {
	contractAddr := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addrHash := common.BytesToHash(crypto.Keccak256(contractAddr.Bytes()))

	prefix := StoragePrefix(addrHash)

	if len(prefix) != 66 {
		t.Fatalf("StoragePrefix: got %d nibbles, want 66", len(prefix))
	}

	// The dump's shortest storage_trie_nodes key is exactly the 66-nibble prefix
	// (it is the storage-root row). Verify against it.
	dumpRows := loadDump(t, "storage_trie_nodes")
	var shortest []byte
	for _, r := range dumpRows {
		if shortest == nil || len(r.key) < len(shortest) {
			shortest = r.key
		}
	}
	if len(shortest) != 66 {
		t.Fatalf("dump shortest storage key: got %d nibbles, want 66", len(shortest))
	}

	gotHex := hex.EncodeToString(prefix)
	wantHex := hex.EncodeToString(shortest)
	if gotHex != wantHex {
		t.Errorf("StoragePrefix mismatch:\n got  %s\n want %s", gotHex, wantHex)
	}
}

// TestPrefixedSinkGolden drives a Builder through PrefixedSink for the
// contract at 0x2000...0002 and asserts that ALL emitted (key, value) pairs
// byte-match EVERY row in storage_trie_nodes. This is stronger than the
// Phase 1a TestStorageTrieGolden which manually prefixed inside the sink;
// here the prefix is applied by PrefixedSink itself.
func TestPrefixedSinkGolden(t *testing.T) {
	contractAddr := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addrHash := common.BytesToHash(crypto.Keccak256(contractAddr.Bytes()))

	// Storage slots: slot 0->0xaa, slot 1->0xbb, slot 2->0xcccccc.
	slot0Key := crypto.Keccak256(make([]byte, 32))
	slot1Key := func() []byte {
		b := make([]byte, 32)
		b[31] = 1
		return crypto.Keccak256(b)
	}()
	slot2Key := func() []byte {
		b := make([]byte, 32)
		b[31] = 2
		return crypto.Keccak256(b)
	}()

	valAA, _ := uint256.FromBig(big.NewInt(0xaa))
	valBB, _ := uint256.FromBig(big.NewInt(0xbb))
	valCC, _ := uint256.FromBig(new(big.Int).SetBytes(hexBytes("cccccc")))

	type leaf struct {
		key []byte
		val []byte
	}
	leaves := []leaf{
		{BytesToNibbles(slot0Key), EncodeStorageValue(valAA)},
		{BytesToNibbles(slot1Key), EncodeStorageValue(valBB)},
		{BytesToNibbles(slot2Key), EncodeStorageValue(valCC)},
	}
	sort.Slice(leaves, func(i, j int) bool {
		return string(leaves[i].key) < string(leaves[j].key)
	})

	// Collect rows emitted via PrefixedSink (keys already include the prefix).
	type row struct {
		key []byte
		val []byte
	}
	var emitted []row
	rawSink := func(path []byte, val []byte) error {
		k := make([]byte, len(path))
		copy(k, path)
		v := make([]byte, len(val))
		copy(v, val)
		emitted = append(emitted, row{k, v})
		return nil
	}

	b := NewBuilder(PrefixedSink(addrHash, rawSink))
	for _, l := range leaves {
		if err := b.AddLeaf(l.key, l.val); err != nil {
			t.Fatalf("AddLeaf: %v", err)
		}
	}
	root, err := b.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}

	wantRoot := common.HexToHash("0x22dbd2f4a67867d30f89c00ed51a4445723f09b1a1f4949955d80b2046efbfce")
	if root != wantRoot {
		t.Errorf("storage trie root:\n got  %s\n want %s", root.Hex(), wantRoot.Hex())
	}

	// Load the dump and compare every row.
	dumpRows := loadDump(t, "storage_trie_nodes")
	dumpMap := make(map[string]string, len(dumpRows))
	for _, r := range dumpRows {
		dumpMap[hex.EncodeToString(r.key)] = hex.EncodeToString(r.val)
	}

	if len(emitted) != len(dumpRows) {
		t.Errorf("storage rows count: got %d, want %d", len(emitted), len(dumpRows))
	}
	for _, r := range emitted {
		keyHex := hex.EncodeToString(r.key)
		wantVal, ok := dumpMap[keyHex]
		if !ok {
			t.Errorf("unexpected storage row key: %s", keyHex)
			continue
		}
		gotVal := hex.EncodeToString(r.val)
		if gotVal != wantVal {
			t.Errorf("storage row %s:\n got  %s\n want %s", keyHex, gotVal, wantVal)
		}
	}
}

// ---------------------------------------------------------------------------
// go-ethereum/trie oracle cross-check
// ---------------------------------------------------------------------------

// TestAccountTrieOracleGolden builds the same 3-account set as
// TestAccountTrieGolden using go-ethereum's trie.StackTrie and asserts:
//  1. The StackTrie root equals 0x7acb3fa2...006fb (the dump's genesis_state_root).
//  2. The StackTrie root equals our internal Builder's root from TestAccountTrieGolden.
//
// This guards against a shared bug where both our codec and the fixture agree
// on a wrong hash. The oracle uses entirely separate MPT code from ours.
//
// Note: go-ethereum/trie is imported ONLY in this _test.go file, never in
// production .go files (enforced by doc.go import constraints).
func TestAccountTrieOracleGolden(t *testing.T) {
	addr1 := common.HexToAddress("0x1000000000000000000000000000000000000001")
	addr2 := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addr3 := common.HexToAddress("0x3000000000000000000000000000000000000003")

	emptyStorage := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	emptyCode := common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
	codeHash2 := common.HexToHash("0xe7562a3f4c743ac17ba863e2265dc8b2233ab92c81c47c18cea261f588a45b8d")
	codeHash3 := common.HexToHash("0x990b8e8fb1baa23bac1161263585add033b872e524deb7467965b234d1e24356")
	storageRoot2 := common.HexToHash("0x22dbd2f4a67867d30f89c00ed51a4445723f09b1a1f4949955d80b2046efbfce")

	bal1, _ := uint256.FromBig(new(big.Int).SetBytes(hexBytes("3635c9adc5dea00000")))
	enc1 := EncodeAccountState(7, bal1, emptyStorage, emptyCode)

	bal2, _ := uint256.FromBig(big.NewInt(0x64))
	enc2 := EncodeAccountState(1, bal2, storageRoot2, codeHash2)

	bal3 := uint256.NewInt(0)
	enc3 := EncodeAccountState(0, bal3, emptyStorage, codeHash3)

	// Build leaves sorted by keccak256(address) so they are in ascending key order.
	type leaf struct {
		key []byte // raw 32 bytes (keccak256(addr))
		val []byte // RLP-encoded AccountState
	}
	leaves := []leaf{
		{crypto.Keccak256(addr1.Bytes()), enc1},
		{crypto.Keccak256(addr2.Bytes()), enc2},
		{crypto.Keccak256(addr3.Bytes()), enc3},
	}
	sort.Slice(leaves, func(i, j int) bool {
		// Compare 32-byte keys lexicographically.
		for k := 0; k < 32; k++ {
			if leaves[i].key[k] != leaves[j].key[k] {
				return leaves[i].key[k] < leaves[j].key[k]
			}
		}
		return false
	})

	// Feed into go-ethereum StackTrie. Update takes raw key bytes; internally it
	// converts to nibbles via keybytesToHex (which appends terminator 0x10).
	st := trie.NewStackTrie(nil)
	for _, l := range leaves {
		if err := st.Update(l.key, l.val); err != nil {
			t.Fatalf("StackTrie.Update: %v", err)
		}
	}
	oracleRoot := st.Hash()

	wantRoot := common.HexToHash("0x7acb3fa2fa378c411135fef796a61c4c681c14e9c844e4af1d779a6f6a7006fb")
	if oracleRoot != wantRoot {
		t.Errorf("go-ethereum StackTrie root:\n got  %s\n want %s", oracleRoot.Hex(), wantRoot.Hex())
	}

	// Also compare against our internal Builder root (recompute it here).
	builderLeaves := []struct {
		key []byte
		val []byte
	}{
		{BytesToNibbles(leaves[0].key), leaves[0].val},
		{BytesToNibbles(leaves[1].key), leaves[1].val},
		{BytesToNibbles(leaves[2].key), leaves[2].val},
	}
	bld := NewBuilder(nil)
	for _, l := range builderLeaves {
		if err := bld.AddLeaf(l.key, l.val); err != nil {
			t.Fatalf("Builder.AddLeaf: %v", err)
		}
	}
	builderRoot, err := bld.Root()
	if err != nil {
		t.Fatalf("Builder.Root: %v", err)
	}
	if builderRoot != oracleRoot {
		t.Errorf("Builder root != oracle root:\n builder: %s\n oracle:  %s", builderRoot.Hex(), oracleRoot.Hex())
	}
}
