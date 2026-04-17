package generator

import (
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

func TestGenerateSmallState(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  10,
		NumContracts: 5,
		MaxSlots:     100,
		MinSlots:     10,
		Distribution: Uniform,
		Seed:         12345,
		BatchSize:    100,
		Workers:      2,
		CodeSize:     256,
		Verbose:      false,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("Failed to create generator: %v", err)
	}
	defer gen.Close()

	stats, err := gen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate state: %v", err)
	}

	// Verify statistics
	if stats.AccountsCreated != 10 {
		t.Errorf("Expected 10 accounts, got %d", stats.AccountsCreated)
	}
	if stats.ContractsCreated != 5 {
		t.Errorf("Expected 5 contracts, got %d", stats.ContractsCreated)
	}
	if stats.StorageSlotsCreated < 50 { // 5 contracts * 10 min slots
		t.Errorf("Expected at least 50 storage slots, got %d", stats.StorageSlotsCreated)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Error("State root should not be empty")
	}
	if stats.TotalBytes == 0 {
		t.Error("Total bytes should not be zero")
	}
}

func TestStorageDistributions(t *testing.T) {
	distributions := []struct {
		name string
		dist Distribution
	}{
		{"PowerLaw", PowerLaw},
		{"Uniform", Uniform},
		{"Exponential", Exponential},
	}

	for _, tc := range distributions {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "testdb")

			config := Config{
				DBPath:       dbPath,
				NumAccounts:  5,
				NumContracts: 20,
				MaxSlots:     1000,
				MinSlots:     1,
				Distribution: tc.dist,
				Seed:         42,
				BatchSize:    100,
				Workers:      2,
				CodeSize:     128,
				Verbose:      false,
			}

			gen, err := New(config)
			if err != nil {
				t.Fatalf("Failed to create generator: %v", err)
			}
			defer gen.Close()

			stats, err := gen.Generate()
			if err != nil {
				t.Fatalf("Failed to generate state: %v", err)
			}

			// Basic sanity checks
			if stats.StorageSlotsCreated < 20 { // At least 1 slot per contract
				t.Errorf("Expected at least 20 storage slots, got %d", stats.StorageSlotsCreated)
			}
		})
	}
}

func TestDatabaseContent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  3,
		NumContracts: 2,
		MaxSlots:     10,
		MinSlots:     5,
		Distribution: Uniform,
		Seed:         99,
		BatchSize:    100,
		Workers:      1,
		CodeSize:     64,
		Verbose:      false,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("Failed to create generator: %v", err)
	}

	stats, err := gen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate state: %v", err)
	}
	gen.Close()

	// Reopen database and verify content
	db, err := pebble.New(dbPath, 64, 64, "verify/", true)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()

	// Verify snapshot root was written
	snapshotRootKey := []byte("SnapshotRoot")
	snapshotRootData, err := db.Get(snapshotRootKey)
	if err != nil {
		t.Fatalf("Failed to read snapshot root: %v", err)
	}
	snapshotRoot := common.BytesToHash(snapshotRootData)
	if snapshotRoot == (common.Hash{}) {
		t.Error("Snapshot root should be set")
	}
	if snapshotRoot != stats.StateRoot {
		t.Errorf("Snapshot root mismatch: got %s, want %s", snapshotRoot.Hex(), stats.StateRoot.Hex())
	}

	// Count account snapshots
	iter := db.NewIterator([]byte("a"), nil)
	accountCount := 0
	for iter.Next() {
		accountCount++
	}
	iter.Release()

	expectedAccounts := config.NumAccounts + config.NumContracts
	if accountCount != expectedAccounts {
		t.Errorf("Expected %d accounts in DB, got %d", expectedAccounts, accountCount)
	}

	// Count storage snapshots
	iter = db.NewIterator([]byte("o"), nil)
	storageCount := 0
	for iter.Next() {
		storageCount++
	}
	iter.Release()

	if storageCount != stats.StorageSlotsCreated {
		t.Errorf("Expected %d storage slots in DB, got %d", stats.StorageSlotsCreated, storageCount)
	}

	// Count code entries
	iter = db.NewIterator([]byte("c"), nil)
	codeCount := 0
	for iter.Next() {
		codeCount++
	}
	iter.Release()

	if codeCount != config.NumContracts {
		t.Errorf("Expected %d code entries in DB, got %d", config.NumContracts, codeCount)
	}
}

func TestStorageValueEncoding(t *testing.T) {
	tests := []struct {
		name     string
		value    common.Hash
		expected []byte
	}{
		{
			name:     "small value",
			value:    common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
			expected: mustRLP([]byte{0x01}),
		},
		{
			name:     "medium value",
			value:    common.HexToHash("0x00000000000000000000000000000000000000000000000000000000deadbeef"),
			expected: mustRLP([]byte{0xde, 0xad, 0xbe, 0xef}),
		},
		{
			name:     "full value",
			value:    common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
			expected: mustRLP(common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff").Bytes()),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeStorageValue(tc.value)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if string(encoded) != string(tc.expected) {
				t.Errorf("Encoding mismatch for %s: got %x, want %x", tc.name, encoded, tc.expected)
			}
		})
	}
}

func TestKeyEncoding(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addrHash := crypto.Keccak256Hash(addr[:])

	// Test account snapshot key
	accKey := gethAccountSnapshotKey(addrHash)
	if len(accKey) != 1+common.HashLength {
		t.Errorf("Account key wrong length: got %d, want %d", len(accKey), 1+common.HashLength)
	}
	if accKey[0] != 'a' {
		t.Errorf("Account key wrong prefix: got %c, want 'a'", accKey[0])
	}

	// Test storage snapshot key
	storageKey := common.HexToHash("0xabcdef")
	storageKeyHash := crypto.Keccak256Hash(storageKey[:])
	stoKey := gethStorageSnapshotKey(addrHash, storageKeyHash)
	if len(stoKey) != 1+common.HashLength*2 {
		t.Errorf("Storage key wrong length: got %d, want %d", len(stoKey), 1+common.HashLength*2)
	}
	if stoKey[0] != 'o' {
		t.Errorf("Storage key wrong prefix: got %c, want 'o'", stoKey[0])
	}

	// Test code key
	codeHash := crypto.Keccak256Hash([]byte("test code"))
	cKey := gethCodeKey(codeHash)
	if len(cKey) != 1+common.HashLength {
		t.Errorf("Code key wrong length: got %d, want %d", len(cKey), 1+common.HashLength)
	}
	if cKey[0] != 'c' {
		t.Errorf("Code key wrong prefix: got %c, want 'c'", cKey[0])
	}
}

func TestReproducibility(t *testing.T) {
	// Generate twice with same seed, results should be identical
	var roots [2]common.Hash

	for i := 0; i < 2; i++ {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "testdb")

		config := Config{
			DBPath:       dbPath,
			NumAccounts:  10,
			NumContracts: 5,
			MaxSlots:     50,
			MinSlots:     10,
			Distribution: PowerLaw,
			Seed:         54321,
			BatchSize:    100,
			Workers:      1, // Single worker for determinism
			CodeSize:     128,
			Verbose:      false,
		}

		gen, err := New(config)
		if err != nil {
			t.Fatalf("Failed to create generator: %v", err)
		}

		stats, err := gen.Generate()
		if err != nil {
			t.Fatalf("Failed to generate state: %v", err)
		}
		gen.Close()

		roots[i] = stats.StateRoot
	}

	if roots[0] != roots[1] {
		t.Errorf("State roots should be identical with same seed: %s != %s", roots[0].Hex(), roots[1].Hex())
	}
}

func mustRLP(data []byte) []byte {
	encoded, err := rlp.EncodeToBytes(data)
	if err != nil {
		panic(err)
	}
	return encoded
}

// Benchmarks

func BenchmarkGenerate1K(b *testing.B) {
	benchmarkGenerate(b, 100, 10, 100)
}

func BenchmarkGenerate10K(b *testing.B) {
	benchmarkGenerate(b, 1000, 100, 100)
}

func BenchmarkGenerate100K(b *testing.B) {
	benchmarkGenerate(b, 10000, 1000, 100)
}

func BenchmarkGenerate1M(b *testing.B) {
	benchmarkGenerate(b, 10000, 10000, 100)
}

func benchmarkGenerate(b *testing.B, accounts, contracts, maxSlots int) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		tmpDir, err := os.MkdirTemp("", "stategen-bench-*")
		if err != nil {
			b.Fatal(err)
		}
		dbPath := filepath.Join(tmpDir, "testdb")

		config := Config{
			DBPath:       dbPath,
			NumAccounts:  accounts,
			NumContracts: contracts,
			MaxSlots:     maxSlots,
			MinSlots:     10,
			Distribution: PowerLaw,
			Seed:         int64(i),
			BatchSize:    10000,
			Workers:      4,
			CodeSize:     512,
			Verbose:      false,
		}

		gen, err := New(config)
		if err != nil {
			b.Fatal(err)
		}

		stats, err := gen.Generate()
		if err != nil {
			b.Fatal(err)
		}
		gen.Close()

		b.ReportMetric(float64(stats.StorageSlotsCreated), "slots")
		b.ReportMetric(float64(stats.TotalBytes)/1024/1024, "MB")

		os.RemoveAll(tmpDir)
	}
}

// --- Binary Trie Tests ---

func TestGenerateBinaryTrie(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  10,
		NumContracts: 5,
		MaxSlots:     100,
		MinSlots:     1,
		Distribution: PowerLaw,
		Seed:         12345,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     256,
		TrieMode:     TrieModeBinary,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("Failed to create generator: %v", err)
	}
	defer gen.Close()

	stats, err := gen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate state: %v", err)
	}

	if stats.AccountsCreated != 10 {
		t.Errorf("Expected 10 accounts, got %d", stats.AccountsCreated)
	}
	if stats.ContractsCreated != 5 {
		t.Errorf("Expected 5 contracts, got %d", stats.ContractsCreated)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Error("State root should not be empty")
	}
	if stats.TotalBytes == 0 {
		t.Error("Total bytes should not be zero")
	}

	// Verify binary trie produces a different root than MPT with the same seed
	mptDir := t.TempDir()
	mptConfig := config
	mptConfig.DBPath = filepath.Join(mptDir, "testdb-mpt")
	mptConfig.TrieMode = TrieModeMPT

	mptGen, err := New(mptConfig)
	if err != nil {
		t.Fatalf("Failed to create MPT generator: %v", err)
	}
	defer mptGen.Close()

	mptStats, err := mptGen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate MPT state: %v", err)
	}

	if stats.StateRoot == mptStats.StateRoot {
		t.Error("Binary trie and MPT should produce different state roots with the same seed")
	}
}

func TestDatabaseContentBinaryTrie(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  3,
		NumContracts: 2,
		MaxSlots:     10,
		MinSlots:     5,
		Distribution: Uniform,
		Seed:         99,
		BatchSize:    100,
		Workers:      1,
		CodeSize:     64,
		TrieMode:     TrieModeBinary,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("Failed to create generator: %v", err)
	}

	stats, err := gen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate state: %v", err)
	}
	gen.Close()

	// Reopen and verify content
	db, err := pebble.New(dbPath, 64, 64, "verify/", true)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()

	// Verify snapshot root
	snapshotRootData, err := db.Get([]byte("SnapshotRoot"))
	if err != nil {
		t.Fatalf("Failed to read snapshot root: %v", err)
	}
	snapshotRoot := common.BytesToHash(snapshotRootData)
	if snapshotRoot != stats.StateRoot {
		t.Errorf("Snapshot root mismatch: got %s, want %s", snapshotRoot.Hex(), stats.StateRoot.Hex())
	}

	// MPT-style snapshot entries ("a", "o") should NOT exist in binary trie mode.
	// Flat state is stored as stem blobs under "vX" prefix instead.
	iter := db.NewIterator([]byte("a"), nil)
	accountCount := 0
	for iter.Next() {
		accountCount++
	}
	iter.Release()
	if accountCount != 0 {
		t.Errorf("Expected 0 MPT account snapshots in binary trie mode, got %d", accountCount)
	}

	iter = db.NewIterator([]byte("o"), nil)
	storageCount := 0
	for iter.Next() {
		storageCount++
	}
	iter.Release()
	if storageCount != 0 {
		t.Errorf("Expected 0 MPT storage snapshots in binary trie mode, got %d", storageCount)
	}

	// Verify stem blobs exist under "vX" prefix.
	iter = db.NewIterator([]byte("vX"), nil)
	stemBlobCount := 0
	for iter.Next() {
		key := iter.Key()
		val := iter.Value()
		// Key must be "vX" + stem(31 bytes) = 33 bytes total
		if len(key) != len(binTrieFlatStatePrefix)+stemSize {
			t.Errorf("Unexpected stem blob key length: got %d, want %d", len(key), len(binTrieFlatStatePrefix)+stemSize)
		}
		// Value must be bitmap(32) + N*32 bytes where N = popcount(bitmap)
		if len(val) < hashSize {
			t.Errorf("Stem blob too short: got %d bytes, want at least %d", len(val), hashSize)
			continue
		}
		var bitmap [hashSize]byte
		copy(bitmap[:], val[:hashSize])
		popcount := 0
		for _, b := range bitmap {
			popcount += bits.OnesCount8(b)
		}
		expectedLen := hashSize + popcount*hashSize
		if len(val) != expectedLen {
			t.Errorf("Stem blob length mismatch: got %d, want %d (popcount=%d)", len(val), expectedLen, popcount)
		}
		stemBlobCount++
	}
	iter.Release()

	if stemBlobCount == 0 {
		t.Errorf("Expected stem blobs under 'vX' prefix, got 0")
	}
	t.Logf("Found %d stem blobs under 'vX' prefix", stemBlobCount)

	// Verify StemBlobBytes stat is populated
	if stats.StemBlobBytes == 0 {
		t.Errorf("Expected StemBlobBytes > 0, got 0")
	}

	// Count code entries (prefix "c") — still written in binary trie mode
	iter = db.NewIterator([]byte("c"), nil)
	codeCount := 0
	for iter.Next() {
		codeCount++
	}
	iter.Release()

	if codeCount != config.NumContracts {
		t.Errorf("Expected %d code entries in DB, got %d", config.NumContracts, codeCount)
	}
}

func TestStemBlobEncoding(t *testing.T) {
	// Verify serializeStemBlob produces the expected format:
	// [bitmap(32)][value0(32)][value1(32)]...
	// Bitmap: byte offset/8, MSB = lowest in-byte offset.

	entries := []trieEntry{
		// Offset 0 (BasicData) — byte 0, bit 7
		{Key: [32]byte{31: 0}, Value: [32]byte{0: 0xAA}},
		// Offset 1 (CodeHash) — byte 0, bit 6
		{Key: [32]byte{31: 1}, Value: [32]byte{0: 0xBB}},
		// Offset 128 (code chunk 0) — byte 16, bit 7
		{Key: [32]byte{31: 128}, Value: [32]byte{0: 0xCC}},
	}

	blob := serializeStemBlob(entries)

	// Expected size: 32 (bitmap) + 3*32 (values) = 128
	if len(blob) != 128 {
		t.Fatalf("Expected blob length 128, got %d", len(blob))
	}

	// Check bitmap bits
	bitmap := blob[:32]
	// Offset 0: byte 0, bit 7 (0x80)
	// Offset 1: byte 0, bit 6 (0x40)
	// Combined byte 0 = 0xC0
	if bitmap[0] != 0xC0 {
		t.Errorf("Bitmap byte 0: got 0x%02X, want 0xC0", bitmap[0])
	}
	// Offset 128: byte 16, bit 7 (0x80)
	if bitmap[16] != 0x80 {
		t.Errorf("Bitmap byte 16: got 0x%02X, want 0x80", bitmap[16])
	}
	// All other bitmap bytes should be 0
	for i, b := range bitmap {
		if i == 0 || i == 16 {
			continue
		}
		if b != 0 {
			t.Errorf("Bitmap byte %d should be 0, got 0x%02X", i, b)
		}
	}

	// Check values are packed in order
	val0 := blob[32:64]
	val1 := blob[64:96]
	val2 := blob[96:128]
	if val0[0] != 0xAA {
		t.Errorf("Value 0 first byte: got 0x%02X, want 0xAA", val0[0])
	}
	if val1[0] != 0xBB {
		t.Errorf("Value 1 first byte: got 0x%02X, want 0xBB", val1[0])
	}
	if val2[0] != 0xCC {
		t.Errorf("Value 2 first byte: got 0x%02X, want 0xCC", val2[0])
	}
}

func TestBinaryTrieReproducibility(t *testing.T) {
	var roots [2]common.Hash

	for i := 0; i < 2; i++ {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "testdb")

		config := Config{
			DBPath:       dbPath,
			NumAccounts:  10,
			NumContracts: 5,
			MaxSlots:     50,
			MinSlots:     10,
			Distribution: PowerLaw,
			Seed:         54321,
			BatchSize:    100,
			Workers:      1,
			CodeSize:     128,
			TrieMode:     TrieModeBinary,
		}

		gen, err := New(config)
		if err != nil {
			t.Fatalf("Failed to create generator: %v", err)
		}

		stats, err := gen.Generate()
		if err != nil {
			t.Fatalf("Failed to generate state: %v", err)
		}
		gen.Close()

		roots[i] = stats.StateRoot
	}

	if roots[0] != roots[1] {
		t.Errorf("Binary trie state roots should be identical with same seed: %s != %s",
			roots[0].Hex(), roots[1].Hex())
	}
}

func TestBinaryTrieStateRootValue(t *testing.T) {
	// Golden value test: pin the binary trie state root for a specific configuration.
	// If the upstream bintrie API changes behavior, this test fails loudly.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  10,
		NumContracts: 5,
		MaxSlots:     100,
		MinSlots:     1,
		Distribution: PowerLaw,
		Seed:         12345,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     256,
		TrieMode:     TrieModeBinary,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("Failed to create generator: %v", err)
	}
	defer gen.Close()

	stats, err := gen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate state: %v", err)
	}

	expected := common.HexToHash("0xee656cf3921d8cbd1aa003a881128846feed2f2c670fa9110cac78f6f8e9d263")
	if stats.StateRoot != expected {
		t.Errorf("Binary trie state root mismatch:\n  got:  %s\n  want: %s\nThis may indicate an upstream bintrie API change.",
			stats.StateRoot.Hex(), expected.Hex())
	}
}

// --- Incremental Commit Tests ---

// TestBinaryTrieCommitIntervalRootEquivalence verifies that incremental
// disk-backed commits produce the exact same state root as the default
// all-in-memory approach. This is the key correctness invariant for the
// CommitInterval feature: the commit→reopen→continue cycle must not alter
// the final trie hash.
func TestBinaryTrieCommitIntervalRootEquivalence(t *testing.T) {
	// Use a configuration with enough accounts and contracts that multiple
	// commit cycles are triggered at CommitInterval=5.
	baseConfig := Config{
		NumAccounts:  100,
		NumContracts: 10,
		MaxSlots:     50,
		MinSlots:     1,
		Distribution: PowerLaw,
		Seed:         7777,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     128,
		TrieMode:     TrieModeBinary,
	}

	// Run 1: all in-memory (CommitInterval=0, default behavior).
	inMemDir := t.TempDir()
	inMemConfig := baseConfig
	inMemConfig.DBPath = filepath.Join(inMemDir, "db")
	inMemConfig.CommitInterval = 0

	gen1, err := New(inMemConfig)
	if err != nil {
		t.Fatalf("Failed to create in-memory generator: %v", err)
	}
	stats1, err := gen1.Generate()
	if err != nil {
		t.Fatalf("In-memory generation failed: %v", err)
	}
	gen1.Close()

	// Run 2: incremental commits every 5 accounts.
	commitDir := t.TempDir()
	commitConfig := baseConfig
	commitConfig.DBPath = filepath.Join(commitDir, "db")
	commitConfig.CommitInterval = 5

	gen2, err := New(commitConfig)
	if err != nil {
		t.Fatalf("Failed to create commit-interval generator: %v", err)
	}
	stats2, err := gen2.Generate()
	if err != nil {
		t.Fatalf("Commit-interval generation failed: %v", err)
	}
	gen2.Close()

	// The state roots MUST be identical.
	if stats1.StateRoot != stats2.StateRoot {
		t.Errorf("State root mismatch between in-memory and commit-interval:\n  in-memory:       %s\n  commit-interval: %s",
			stats1.StateRoot.Hex(), stats2.StateRoot.Hex())
	}

	// Also verify the same number of accounts, contracts, and slots.
	if stats1.AccountsCreated != stats2.AccountsCreated {
		t.Errorf("AccountsCreated mismatch: %d vs %d", stats1.AccountsCreated, stats2.AccountsCreated)
	}
	if stats1.ContractsCreated != stats2.ContractsCreated {
		t.Errorf("ContractsCreated mismatch: %d vs %d", stats1.ContractsCreated, stats2.ContractsCreated)
	}
	if stats1.StorageSlotsCreated != stats2.StorageSlotsCreated {
		t.Errorf("StorageSlotsCreated mismatch: %d vs %d", stats1.StorageSlotsCreated, stats2.StorageSlotsCreated)
	}

	t.Logf("Root equivalence confirmed: %s (in-memory) == %s (commit-interval=5)",
		stats1.StateRoot.Hex(), stats2.StateRoot.Hex())
	t.Logf("Stats: %d accounts, %d contracts, %d storage slots",
		stats1.AccountsCreated, stats1.ContractsCreated, stats1.StorageSlotsCreated)
}

// TestBinaryTrieCommitIntervalGoldenHash verifies that the golden hash test
// passes identically with CommitInterval>0. This ensures the incremental
// commit path produces the exact same pinned hash as CommitInterval=0.
func TestBinaryTrieCommitIntervalGoldenHash(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:         dbPath,
		NumAccounts:    10,
		NumContracts:   5,
		MaxSlots:       100,
		MinSlots:       1,
		Distribution:   PowerLaw,
		Seed:           12345,
		BatchSize:      1000,
		Workers:        1,
		CodeSize:       256,
		TrieMode:       TrieModeBinary,
		CommitInterval: 3, // Commit every 3 accounts
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("Failed to create generator: %v", err)
	}
	defer gen.Close()

	stats, err := gen.Generate()
	if err != nil {
		t.Fatalf("Failed to generate state: %v", err)
	}

	// Must match the same golden hash as TestBinaryTrieStateRootValue.
	expected := common.HexToHash("0xee656cf3921d8cbd1aa003a881128846feed2f2c670fa9110cac78f6f8e9d263")
	if stats.StateRoot != expected {
		t.Errorf("CommitInterval golden hash mismatch:\n  got:  %s\n  want: %s",
			stats.StateRoot.Hex(), expected.Hex())
	}
}

func TestTargetSizeStopsEarly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	// Generate without target-size to get a baseline.
	// Use enough contracts (5000) so that Pebble flushes data to disk,
	// making dirSize() measurements meaningful for the target check.
	configFull := Config{
		DBPath:       dbPath,
		NumAccounts:  20,
		NumContracts: 5000,
		MaxSlots:     100,
		MinSlots:     10,
		Distribution: PowerLaw,
		Seed:         42,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     256,
		TrieMode:     TrieModeBinary,
	}

	genFull, err := New(configFull)
	if err != nil {
		t.Fatalf("Failed to create full generator: %v", err)
	}
	statsFull, err := genFull.Generate()
	if err != nil {
		t.Fatalf("Failed to generate full state: %v", err)
	}
	genFull.Close()

	fullContracts := statsFull.ContractsCreated
	if fullContracts < 100 {
		t.Skipf("Full run only created %d contracts, not enough to test early stop", fullContracts)
	}

	dbPath2 := filepath.Join(tmpDir, "testdb2")
	// Use a target size (~500KB) that should cause early stopping well
	// before all 5000 contracts are generated.
	configTarget := Config{
		DBPath:       dbPath2,
		NumAccounts:  20,
		NumContracts: 5000,
		MaxSlots:     100,
		MinSlots:     10,
		Distribution: PowerLaw,
		Seed:         42,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     256,
		TrieMode:     TrieModeBinary,
		TargetSize:   500 * 1024, // 500 KB target
	}

	genTarget, err := New(configTarget)
	if err != nil {
		t.Fatalf("Failed to create target generator: %v", err)
	}
	statsTarget, err := genTarget.Generate()
	if err != nil {
		t.Fatalf("Failed to generate target state: %v", err)
	}
	genTarget.Close()

	t.Logf("Full: %d contracts, %d slots", statsFull.ContractsCreated, statsFull.StorageSlotsCreated)
	t.Logf("Target (500KB): %d contracts, %d slots", statsTarget.ContractsCreated, statsTarget.StorageSlotsCreated)

	if statsTarget.ContractsCreated >= statsFull.ContractsCreated {
		t.Errorf("Target-size should have stopped early: created %d contracts (full run: %d)",
			statsTarget.ContractsCreated, statsFull.ContractsCreated)
	}

	// The target run should still produce a valid state root.
	if statsTarget.StateRoot == (common.Hash{}) {
		t.Error("Target run produced empty state root")
	}
	// NB: this legacy test only asserts "stops before full run"; it does NOT
	// assert the final DB size is near 500 KB (it isn't — Pebble WAL/memtable
	// overhead dominates at this target size, producing ~10+ MB). The
	// accuracy regression fence lives in TestTargetSizeStopsAccurately_Bintrie
	// at a larger target where noise is negligible.
}

// assertDBSizeWithin fails the test if the on-disk size of dbPath differs
// from target by more than tolerance (a fraction, e.g. 0.35 for ±35%).
// Uses the same filesystem walk that main.go:408 uses for its post-run report,
// so the assertion reflects what an operator would see after the run.
func assertDBSizeWithin(t *testing.T, dbPath string, target uint64, tolerance float64) {
	t.Helper()
	actual, err := dirSize(dbPath)
	if err != nil {
		t.Fatalf("dirSize(%q): %v", dbPath, err)
	}
	diff := float64(actual) - float64(target)
	if diff < 0 {
		diff = -diff
	}
	ratio := diff / float64(target)
	t.Logf("DB size check: actual=%d target=%d diff=%.1f%% tolerance=%.1f%%",
		actual, target, ratio*100, tolerance*100)
	if ratio > tolerance {
		t.Errorf("DB size %.1f%% off target (%d vs %d), tolerance %.1f%%",
			ratio*100, actual, target, tolerance*100)
	}
}

// TestTargetSizeStopsAccurately_Bintrie is the regression fence for the
// bintrie stop-condition refactor. Pre-refactor, the 3-tier heuristic with
// bytesPerEntry=80 (vs observed ~130) overshoots a 10 MB target by 14× or
// more. After C3's single in-memory estimator with FinalSizeFactor=2.03,
// the run lands within ~40% of target at this scale — the remainder is
// Pebble compression variance that grows with scale (the 2.03 default is
// calibrated from a 2.9 GB / 22.4M-entry run where compression is more
// efficient due to deeper prefix sharing). Operators wanting tighter
// accuracy at a specific workload can use the calibration log (C4) to
// compute a workload-specific --final-size-factor.
func TestTargetSizeStopsAccurately_Bintrie(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long target-size test in -short mode")
	}
	const target uint64 = 50 * 1024 * 1024 // 50 MB (reduces small-scale overhead)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  20,
		NumContracts: 1_000_000, // large upper bound so TargetSize governs
		MaxSlots:     100,
		MinSlots:     10,
		Distribution: PowerLaw,
		Seed:         42,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     256,
		TrieMode:     TrieModeBinary,
		TargetSize:   target,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stats, err := gen.Generate()
	if err != nil {
		gen.Close()
		t.Fatalf("Generate: %v", err)
	}
	gen.Close()

	t.Logf("bintrie target=%s: %d contracts, %d slots, root=%s",
		fmtBytes(target), stats.ContractsCreated, stats.StorageSlotsCreated,
		stats.StateRoot.Hex())
	assertDBSizeWithin(t, dbPath, target, 0.35)
}

// TestTargetSizeStopsAccurately_MPT is the sanity check for the MPT path.
// MPT overshoot is bounded by the memtable+WAL slack (~128 MB worst case),
// so a 10 MB target with a loose 0.5 tolerance should pass on both the
// current code and after C6 if/when MPT is migrated to the same estimator.
func TestTargetSizeStopsAccurately_MPT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long target-size test in -short mode")
	}
	const target uint64 = 10 * 1024 * 1024
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "testdb")

	config := Config{
		DBPath:       dbPath,
		NumAccounts:  20,
		NumContracts: 1_000_000,
		MaxSlots:     100,
		MinSlots:     10,
		Distribution: PowerLaw,
		Seed:         43,
		BatchSize:    1000,
		Workers:      1,
		CodeSize:     256,
		TrieMode:     TrieModeMPT,
		TargetSize:   target,
	}

	gen, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stats, err := gen.Generate()
	if err != nil {
		gen.Close()
		t.Fatalf("Generate: %v", err)
	}
	gen.Close()

	t.Logf("MPT target=%s: %d contracts, %d slots", fmtBytes(target), stats.ContractsCreated, stats.StorageSlotsCreated)
	assertDBSizeWithin(t, dbPath, target, 0.5)
}

// TestTargetSizeStopsPreambleIndependence verifies that the stop condition is
// driven by contract-loop entries only, not by preamble (genesis/inject/EOA)
// entries. The currently-committed code increments a shared entryCount in
// writeEntries for all four call sites, so tier 2 trips earlier when the
// preamble is large — producing fewer contracts than an equivalent run
// without preamble. After C2 (preamble counter split), the contract count
// should be within 15% regardless of preamble size.
//
// The test explicitly sets TargetEntries to engage tier 2 on the current
// 3-tier design; after C3 (single estimator), TargetEntries becomes a
// no-op field and this test still passes because the contract-level byte
// counter is the authoritative signal.
func TestTargetSizeStopsPreambleIndependence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping preamble-independence test in -short mode")
	}

	baseConfig := func(dbPath string) Config {
		return Config{
			DBPath:        dbPath,
			NumAccounts:   0,
			NumContracts:  200, // low so targetCheckInterval = 40
			MaxSlots:      10,
			MinSlots:      2,
			Distribution:  PowerLaw,
			Seed:          44,
			BatchSize:     1000,
			Workers:       1,
			CodeSize:      256,
			TrieMode:      TrieModeBinary,
			TargetSize:    1 * 1024 * 1024, // 1 MB (any > 0 engages the check)
			TargetEntries: 1200,            // engages tier 2 on current code
		}
	}

	// Baseline — no genesis preamble.
	tmpA := t.TempDir()
	cfgA := baseConfig(filepath.Join(tmpA, "a"))
	gA, err := New(cfgA)
	if err != nil {
		t.Fatalf("baseline New: %v", err)
	}
	statsA, err := gA.Generate()
	if err != nil {
		gA.Close()
		t.Fatalf("baseline Generate: %v", err)
	}
	gA.Close()

	// With genesis preamble — 500 accounts + 3 storage slots each ≈ 1500
	// preamble entries, enough to exceed TargetEntries=1200 before any
	// contract is processed. Under the bug, this makes tier 2 fire at the
	// first checkpoint (contractIdx=40) with far fewer contracts generated
	// than in the baseline run.
	// Preamble accounts are EOAs (no storage, no code) so they do NOT
	// increment stats.ContractsCreated — keeping the comparison apples-to-apples.
	// Each still calls writeEntries once (account-header entry), which primes
	// the shared entryCount sufficiently to trip tier 2 under the D3 bug.
	genesisAccts := make(map[common.Address]*types.StateAccount, 1500)
	for i := 0; i < 1500; i++ {
		var addr common.Address
		addr[19] = byte(i)
		addr[18] = byte(i >> 8)
		genesisAccts[addr] = &types.StateAccount{
			Balance:  uint256.NewInt(uint64(i + 1)),
			Nonce:    uint64(i),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}
	}

	tmpB := t.TempDir()
	cfgB := baseConfig(filepath.Join(tmpB, "b"))
	cfgB.GenesisAccounts = genesisAccts
	gB, err := New(cfgB)
	if err != nil {
		t.Fatalf("with-preamble New: %v", err)
	}
	statsB, err := gB.Generate()
	if err != nil {
		gB.Close()
		t.Fatalf("with-preamble Generate: %v", err)
	}
	gB.Close()

	t.Logf("baseline: %d contracts; with-preamble: %d contracts",
		statsA.ContractsCreated, statsB.ContractsCreated)

	if statsA.ContractsCreated == 0 {
		t.Fatalf("baseline produced zero contracts; test setup is wrong")
	}
	ratio := float64(statsA.ContractsCreated-statsB.ContractsCreated) / float64(statsA.ContractsCreated)
	if ratio < 0 {
		ratio = -ratio
	}
	if ratio > 0.15 {
		t.Errorf("preamble changed contract count by %.1f%% (baseline=%d, with-preamble=%d); stop condition is polluted by preamble entries",
			ratio*100, statsA.ContractsCreated, statsB.ContractsCreated)
	}
}

// fmtBytes formats a byte count for test logs.
func fmtBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func BenchmarkStorageValueEncoding(b *testing.B) {
	value := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000abcd")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encodeStorageValue(value); err != nil {
			b.Fatal(err)
		}
	}
}
