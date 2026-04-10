package generator

import (
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
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
