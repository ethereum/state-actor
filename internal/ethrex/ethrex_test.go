package ethrex

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("mustDecodeHex(%q): %v", s, err)
	}
	return b
}

func hexBytes(s string) []byte {
	s = strings.TrimPrefix(s, "0x")
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("hexBytes(%q): %v", s, err))
	}
	return b
}

func hexNibbles(s string) []byte {
	s = strings.TrimPrefix(s, "0x")
	nibs := make([]byte, len(s))
	for i, c := range s {
		switch {
		case c >= '0' && c <= '9':
			nibs[i] = byte(c - '0')
		case c >= 'a' && c <= 'f':
			nibs[i] = byte(c-'a') + 10
		case c >= 'A' && c <= 'F':
			nibs[i] = byte(c-'A') + 10
		default:
			panic(fmt.Sprintf("invalid hex char %c in %q", c, s))
		}
	}
	return nibs
}

// ---------------------------------------------------------------------------
// nibbles.go tests
// ---------------------------------------------------------------------------

func TestBytesToNibbles(t *testing.T) {
	tests := []struct {
		input []byte
		want  []byte
	}{
		{[]byte{0x2f}, []byte{0x02, 0x0f}},
		{[]byte{0xab, 0xcd}, []byte{0x0a, 0x0b, 0x0c, 0x0d}},
		{[]byte{}, []byte{}},
		{[]byte{0x00}, []byte{0x00, 0x00}},
	}
	for _, tc := range tests {
		got := BytesToNibbles(tc.input)
		if string(got) != string(tc.want) {
			t.Errorf("BytesToNibbles(%x): got %x, want %x", tc.input, got, tc.want)
		}
	}
}

func TestCompactEncode(t *testing.T) {
	tests := []struct {
		nibbles []byte
		isLeaf  bool
		want    []byte
		desc    string
	}{
		// extension, even length: flag 0x00, then pack pairs.
		{[]byte{0x0e}, false, []byte{0x1e}, "ext-odd-1nibble"},
		// extension, even length (2 nibbles).
		{[]byte{0x0a, 0x0b}, false, []byte{0x00, 0xab}, "ext-even-2nibble"},
		// leaf, even length.
		{[]byte{0x0a, 0x0b}, true, []byte{0x20, 0xab}, "leaf-even-2nibble"},
		// leaf, odd length (1 nibble): 0x30 | nibble.
		{[]byte{0x05}, true, []byte{0x35}, "leaf-odd-1nibble"},
		// empty leaf (even, 0 nibbles).
		{[]byte{}, true, []byte{0x20}, "leaf-empty"},
		// empty extension.
		{[]byte{}, false, []byte{0x00}, "ext-empty"},
	}
	for _, tc := range tests {
		got := CompactEncode(tc.nibbles, tc.isLeaf)
		if string(got) != string(tc.want) {
			t.Errorf("CompactEncode(%v, %v) [%s]: got %x, want %x", tc.nibbles, tc.isLeaf, tc.desc, got, tc.want)
		}
	}
}

// A 63-nibble leaf path should produce 32 bytes starting with 0x3X (odd leaf).
func TestCompactEncode63NibbleLeaf(t *testing.T) {
	nibbles := make([]byte, 63)
	for i := range nibbles {
		nibbles[i] = byte(i % 16)
	}
	got := CompactEncode(nibbles, true)
	if len(got) != 32 {
		t.Fatalf("63-nibble leaf compact: got %d bytes, want 32", len(got))
	}
	// Odd leaf: high nibble of first byte must be 3 (0x30 | first_nibble).
	if got[0]>>4 != 3 {
		t.Errorf("63-nibble leaf compact: first byte high nibble = %x, want 3", got[0]>>4)
	}
}

// ---------------------------------------------------------------------------
// node_rlp.go golden checks
// ---------------------------------------------------------------------------

func TestExtensionNodeGolden(t *testing.T) {
	// Extension node at path [3]: CompactEncode([0x0e], false)=0x1e; child hash a031c295...
	// Expected: 0xe21ea031c2957decbef72b30c7fd10f3223f4e79f2e4892fb70b8d084647994ffc4ddb
	childHash := hexBytes("a031c2957decbef72b30c7fd10f3223f4e79f2e4892fb70b8d084647994ffc4ddb")
	// childHash is 33 bytes (0xa0 prefix + 32 byte hash) — this IS the RLP encoding
	// of the child's hash. But EncodeExtension takes the raw child RLP, not the
	// 0xa0-prefixed form.
	// The child at path [0x03,0x0e] is the branch `0xf85180a0067991f1...`.
	// We need to pass the raw child RLP to EncodeExtension.
	// Instead, verify using the branch RLP directly:
	branchRLP := hexBytes("f85180a0067991f14cf9cdff3de8665355e84dda11e311cabf28c07bf6899a694e28421c8080808080808080808080a0d6f55fc8a2c9e74d1d0fed92b0e7357d80607c871217236e649aff95ae4f4c1b808080")
	got := EncodeExtension([]byte{0x0e}, branchRLP)
	want := hexBytes("e21ea031c2957decbef72b30c7fd10f3223f4e79f2e4892fb70b8d084647994ffc4ddb")
	if string(got) != string(want) {
		t.Errorf("EncodeExtension golden:\n got  %x\n want %x", got, want)
	}
	_ = childHash
}

func TestBranchNodeGolden(t *testing.T) {
	// Root branch: 0xf8518080a037920c12c40bc4435a02404eefd126d09b3b03de3b59d272f4e1c1847edf8a44
	//                 a092b62dadae8036935b9ee7ebf3b82b44f069a503a8e63a29f319b0c7165bdfd380808080808080808080808080
	// Slot 2 = keccak256 of the leaf RLP at path 0x02.
	// Slot 3 = keccak256 of the extension RLP at path 0x03.
	// EncodeBranch takes the RAW RLP of each child; it applies NodeRef internally.
	var children [16][]byte
	// Raw leaf RLP at path 0x02 (from genesis_dump.json):
	children[2] = hexBytes("f869a035baa1f53460dfe937af66419cef1b8dd5251c7daa1faf4061b53f21a5cd51e0b846f8440164a022dbd2f4a67867d30f89c00ed51a4445723f09b1a1f4949955d80b2046efbfcea0e7562a3f4c743ac17ba863e2265dc8b2233ab92c81c47c18cea261f588a45b8d")
	// Raw extension RLP at path 0x03 (from genesis_dump.json):
	children[3] = hexBytes("e21ea031c2957decbef72b30c7fd10f3223f4e79f2e4892fb70b8d084647994ffc4ddb")
	got := EncodeBranch(children, nil)
	want := hexBytes("f8518080a037920c12c40bc4435a02404eefd126d09b3b03de3b59d272f4e1c1847edf8a44a092b62dadae8036935b9ee7ebf3b82b44f069a503a8e63a29f319b0c7165bdfd380808080808080808080808080")
	if string(got) != string(want) {
		t.Errorf("EncodeBranch golden:\n got  %x\n want %x", got, want)
	}
}

// ---------------------------------------------------------------------------
// account.go golden checks
// ---------------------------------------------------------------------------

func TestEncodeAccountState(t *testing.T) {
	// EOA: nonce=7, balance=0x3635c9adc5dea00000, emptyStorageRoot, emptyCodeHash
	nonce := uint64(7)
	balance, _ := new(big.Int).SetString("3635c9adc5dea00000", 16)
	bal256, _ := uint256.FromBig(balance)
	emptyStorage := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	emptyCode := common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")

	got := EncodeAccountState(nonce, bal256, emptyStorage, emptyCode)
	want := hexBytes("f84d07893635c9adc5dea00000a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421a0c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
	if string(got) != string(want) {
		t.Errorf("EncodeAccountState:\n got  %x\n want %x", got, want)
	}
}

func TestEncodeStorageValue(t *testing.T) {
	tests := []struct {
		hex  string
		want string
	}{
		{"aa", "81aa"},
		{"bb", "81bb"},
		{"cccccc", "83cccccc"},
	}
	for _, tc := range tests {
		b, _ := new(big.Int).SetString(tc.hex, 16)
		v, _ := uint256.FromBig(b)
		got := EncodeStorageValue(v)
		want := hexBytes(tc.want)
		if string(got) != string(want) {
			t.Errorf("EncodeStorageValue(%s): got %x, want %x", tc.hex, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// code.go golden checks
// ---------------------------------------------------------------------------

func TestComputeJumpTargets(t *testing.T) {
	// 0x60015b00: PUSH1 0x01 (skips byte 1), JUMPDEST at 2, STOP at 3.
	got := ComputeJumpTargets(hexBytes("60015b00"))
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("ComputeJumpTargets(60015b00): got %v, want [2]", got)
	}

	// 0x600160015500: PUSH1 0x01, PUSH1 0x01, SSTORE 0x55, STOP — no JUMPDEST.
	got2 := ComputeJumpTargets(hexBytes("600160015500"))
	if len(got2) != 0 {
		t.Errorf("ComputeJumpTargets(600160015500): got %v, want []", got2)
	}

	// Empty code.
	got3 := ComputeJumpTargets(nil)
	if len(got3) != 0 {
		t.Errorf("ComputeJumpTargets(nil): got %v, want []", got3)
	}
}

func TestEncodeCode(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"60015b00", "8460015b00c102"},
		{"600160015500", "86600160015500c0"},
		{"", "80c0"},
	}
	for _, tc := range tests {
		var code []byte
		if tc.code != "" {
			code = hexBytes(tc.code)
		}
		got := EncodeCode(code)
		want := hexBytes(tc.want)
		if string(got) != string(want) {
			t.Errorf("EncodeCode(%s):\n got  %x\n want %x", tc.code, got, want)
		}
	}
}

func TestCodeLengthMetadata(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"60015b00", "0000000000000004"},
		{"", "0000000000000000"},
		{"600160015500", "0000000000000006"},
	}
	for _, tc := range tests {
		var code []byte
		if tc.code != "" {
			code = hexBytes(tc.code)
		}
		got := CodeLengthMetadata(code)
		want := hexBytes(tc.want)
		if string(got[:]) != string(want) {
			t.Errorf("CodeLengthMetadata(%s): got %x, want %x", tc.code, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// builder.go golden checks — storage trie + account trie vs genesis_dump.json
// ---------------------------------------------------------------------------

// genesisRow is one key/value pair from genesis_dump.json.
type genesisRow struct {
	key []byte
	val []byte
}

// loadDump reads genesis_dump.json and returns the named CF's rows.
func loadDump(t *testing.T, cfName string) []genesisRow {
	t.Helper()
	data, err := os.ReadFile("testdata/genesis_dump.json")
	if err != nil {
		t.Fatalf("read genesis_dump.json: %v", err)
	}
	var dump struct {
		CFs map[string][][2]string `json:"cfs"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatalf("unmarshal genesis_dump.json: %v", err)
	}
	cf, ok := dump.CFs[cfName]
	if !ok {
		t.Fatalf("CF %q not found in dump", cfName)
	}
	rows := make([]genesisRow, len(cf))
	for i, pair := range cf {
		rows[i] = genesisRow{
			key: mustDecodeHex(t, pair[0]),
			val: mustDecodeHex(t, pair[1]),
		}
	}
	return rows
}

// TestStorageTrieGolden builds the storage trie for contract 0x2000...0002's
// 3 slots and verifies the root and rows against genesis_dump.json.
func TestStorageTrieGolden(t *testing.T) {
	// Storage slots from genesis.json:
	//   slot 0 -> 0xaa
	//   slot 1 -> 0xbb
	//   slot 2 -> 0xcccccc
	// Keys = keccak256(big-endian 32-byte slot index).
	slot0Key := crypto.Keccak256(make([]byte, 32)) // keccak256(pad32(0))
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

	// Storage values (ethrex EncodeStorageValue = RLP of minimal big-endian).
	valAA, _ := uint256.FromBig(big.NewInt(0xaa))
	valBB, _ := uint256.FromBig(big.NewInt(0xbb))
	valCC, _ := uint256.FromBig(new(big.Int).SetBytes(hexBytes("cccccc")))
	encAA := EncodeStorageValue(valAA)
	encBB := EncodeStorageValue(valBB)
	encCC := EncodeStorageValue(valCC)

	type leaf struct {
		key []byte
		val []byte
	}
	leaves := []leaf{
		{BytesToNibbles(slot0Key), encAA},
		{BytesToNibbles(slot1Key), encBB},
		{BytesToNibbles(slot2Key), encCC},
	}
	// Sort by nibble key.
	sort.Slice(leaves, func(i, j int) bool {
		return string(leaves[i].key) < string(leaves[j].key)
	})

	// Contract address prefix for storage trie keys.
	contractAddr := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addrHash := crypto.Keccak256(contractAddr.Bytes())
	addrNibs := BytesToNibbles(addrHash)
	// Storage key prefix: addrNibs ++ [LeafFlag] ++ [StorageSeparator]
	storagePrefix := make([]byte, 64+1+1)
	copy(storagePrefix, addrNibs)
	storagePrefix[64] = LeafFlag
	storagePrefix[65] = StorageSeparator

	// Load expected rows from dump.
	dumpRows := loadDump(t, "storage_trie_nodes")
	// Build a map from key-hex to value-hex.
	dumpMap := make(map[string]string)
	for _, r := range dumpRows {
		dumpMap[hex.EncodeToString(r.key)] = hex.EncodeToString(r.val)
	}

	// Collect emitted rows.
	type row struct {
		key []byte
		val []byte
	}
	var emitted []row

	sink := func(path []byte, val []byte) error {
		// Prepend storage prefix.
		full := make([]byte, len(storagePrefix)+len(path))
		copy(full, storagePrefix)
		copy(full[len(storagePrefix):], path)
		v := make([]byte, len(val))
		copy(v, val)
		emitted = append(emitted, row{full, v})
		return nil
	}

	b := NewBuilder(sink)
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

	// Verify emitted rows match dump.
	if len(emitted) != len(dumpRows) {
		t.Errorf("storage trie rows count: got %d, want %d", len(emitted), len(dumpRows))
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

// TestAccountTrieGolden builds the account trie for all 3 genesis accounts
// and verifies root and rows against genesis_dump.json.
func TestAccountTrieGolden(t *testing.T) {
	// Account addresses from genesis.json.
	addr1 := common.HexToAddress("0x1000000000000000000000000000000000000001")
	addr2 := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addr3 := common.HexToAddress("0x3000000000000000000000000000000000000003")

	emptyStorage := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	emptyCode := common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")

	// Code hashes from genesis_dump.json account_codes keys.
	codeHash2 := common.HexToHash("0xe7562a3f4c743ac17ba863e2265dc8b2233ab92c81c47c18cea261f588a45b8d")
	codeHash3 := common.HexToHash("0x990b8e8fb1baa23bac1161263585add033b872e524deb7467965b234d1e24356")

	// Storage root for addr2 (from TestStorageTrieGolden).
	storageRoot2 := common.HexToHash("0x22dbd2f4a67867d30f89c00ed51a4445723f09b1a1f4949955d80b2046efbfce")

	// Account 1: EOA nonce=7 balance=0x3635c9adc5dea00000.
	bal1, _ := uint256.FromBig(new(big.Int).SetBytes(hexBytes("3635c9adc5dea00000")))
	enc1 := EncodeAccountState(7, bal1, emptyStorage, emptyCode)

	// Account 2: nonce=1 balance=0x64 (100), storageRoot2, codeHash2.
	bal2, _ := uint256.FromBig(big.NewInt(0x64))
	enc2 := EncodeAccountState(1, bal2, storageRoot2, codeHash2)

	// Account 3: nonce=0 balance=0, emptyStorage, codeHash3.
	bal3 := uint256.NewInt(0)
	enc3 := EncodeAccountState(0, bal3, emptyStorage, codeHash3)

	// Account keys = keccak256(address).
	key1 := BytesToNibbles(crypto.Keccak256(addr1.Bytes()))
	key2 := BytesToNibbles(crypto.Keccak256(addr2.Bytes()))
	key3 := BytesToNibbles(crypto.Keccak256(addr3.Bytes()))

	type leaf struct {
		key []byte
		val []byte
	}
	leaves := []leaf{
		{key1, enc1},
		{key2, enc2},
		{key3, enc3},
	}
	sort.Slice(leaves, func(i, j int) bool {
		return string(leaves[i].key) < string(leaves[j].key)
	})

	// Load expected rows from dump.
	dumpRows := loadDump(t, "account_trie_nodes")
	dumpMap := make(map[string]string)
	for _, r := range dumpRows {
		dumpMap[hex.EncodeToString(r.key)] = hex.EncodeToString(r.val)
	}

	// Collect emitted rows.
	type row struct {
		key []byte
		val []byte
	}
	var emitted []row
	sink := func(path []byte, val []byte) error {
		k := make([]byte, len(path))
		copy(k, path)
		v := make([]byte, len(val))
		copy(v, val)
		emitted = append(emitted, row{k, v})
		return nil
	}

	bld := NewBuilder(sink)
	for _, l := range leaves {
		if err := bld.AddLeaf(l.key, l.val); err != nil {
			t.Fatalf("AddLeaf: %v", err)
		}
	}
	root, err := bld.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}

	wantRoot := common.HexToHash("0x7acb3fa2fa378c411135fef796a61c4c681c14e9c844e4af1d779a6f6a7006fb")
	if root != wantRoot {
		t.Errorf("account trie root:\n got  %s\n want %s", root.Hex(), wantRoot.Hex())
	}

	// Verify emitted rows match dump.
	if len(emitted) != len(dumpRows) {
		t.Errorf("account trie rows count: got %d, want %d", len(emitted), len(dumpRows))
	}
	for _, r := range emitted {
		keyHex := hex.EncodeToString(r.key)
		wantVal, ok := dumpMap[keyHex]
		if !ok {
			t.Errorf("unexpected account row key %q (val=%x)", keyHex, r.val)
			continue
		}
		gotVal := hex.EncodeToString(r.val)
		if gotVal != wantVal {
			t.Errorf("account row %s:\n got  %s\n want %s", keyHex, gotVal, wantVal)
		}
	}
}

// TestBuilderEmptyTrie verifies the empty trie sentinel row ([] -> 0x80) and root.
func TestBuilderEmptyTrie(t *testing.T) {
	type kv struct{ key, val []byte }
	var rows []kv
	b := NewBuilder(func(path []byte, val []byte) error {
		rows = append(rows, kv{key: append([]byte(nil), path...), val: append([]byte(nil), val...)})
		return nil
	})
	root, err := b.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	wantRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if root != wantRoot {
		t.Errorf("empty trie root: got %s, want %s", root.Hex(), wantRoot.Hex())
	}
	if len(rows) != 1 {
		t.Fatalf("empty trie: want exactly 1 emitted row, got %d", len(rows))
	}
	if len(rows[0].key) != 0 {
		t.Errorf("empty trie sentinel key: got %x, want [] (empty)", rows[0].key)
	}
	if !bytes.Equal(rows[0].val, []byte{0x80}) {
		t.Errorf("empty trie sentinel value: got %x, want 80", rows[0].val)
	}
}

// TestBuilderOutOfOrder verifies that out-of-order keys are rejected.
func TestBuilderOutOfOrder(t *testing.T) {
	b := NewBuilder(nil)
	_ = b.AddLeaf(hexNibbles("0102"), []byte("a"))
	err := b.AddLeaf(hexNibbles("0101"), []byte("b"))
	if err != ErrKeysOutOfOrder {
		t.Errorf("expected ErrKeysOutOfOrder, got %v", err)
	}
}

// TestBuilderMixedKeyLength verifies that keys of differing nibble length are
// rejected (the spine logic indexes key[depth] and assumes uniform length).
func TestBuilderMixedKeyLength(t *testing.T) {
	b := NewBuilder(nil)
	if err := b.AddLeaf(hexNibbles("0102"), []byte("a")); err != nil {
		t.Fatalf("first AddLeaf: %v", err)
	}
	// "010203" is strictly greater than "0102" but longer — must be rejected.
	if err := b.AddLeaf(hexNibbles("010203"), []byte("b")); err == nil {
		t.Errorf("expected mixed-length key rejection, got nil")
	}
}
