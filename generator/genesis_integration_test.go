package generator

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/trie/bintrie"
	"github.com/holiman/uint256"
)

// TestGenesisAccountsIntegration tests that genesis alloc accounts are properly
// included in state generation.
func TestGenesisAccountsIntegration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chaindata")

	// Create some genesis accounts
	genesisAccounts := map[common.Address]*types.StateAccount{
		common.HexToAddress("0x1111111111111111111111111111111111111111"): {
			Nonce:    0,
			Balance:  uint256.NewInt(1e18),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
		common.HexToAddress("0x2222222222222222222222222222222222222222"): {
			Nonce:    5,
			Balance:  uint256.NewInt(2e18),
			Root:     types.EmptyRootHash, // Will be updated
			CodeHash: crypto.Keccak256([]byte{0x60, 0x00}),
		},
	}

	genesisStorage := map[common.Address]map[common.Hash]common.Hash{
		common.HexToAddress("0x2222222222222222222222222222222222222222"): {
			common.HexToHash("0x01"): common.HexToHash("0xdeadbeef"),
			common.HexToHash("0x02"): common.HexToHash("0xcafe"),
		},
	}

	genesisCode := map[common.Address][]byte{
		common.HexToAddress("0x2222222222222222222222222222222222222222"): {0x60, 0x00},
	}

	plan := mustPlan(t, 512<<10)
	config := Config{
		DBPath:          dbPath,
		AutoFill:        plan,
		Seed:            12345,
		Verbose:         false,
		GenesisAccounts: genesisAccounts,
		GenesisStorage:  genesisStorage,
		GenesisCode:     genesisCode,
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

	// Verify stats include genesis accounts: 2 genesis accounts (1 EOA + 1
	// contract) plus plan.NumEOAs/plan.NumContracts synthetic ones.
	expectedAccounts := 1 + plan.NumEOAs
	expectedContracts := 1 + plan.NumContracts

	if stats.AccountsCreated != expectedAccounts {
		t.Errorf("Expected %d accounts, got %d", expectedAccounts, stats.AccountsCreated)
	}
	if stats.ContractsCreated != expectedContracts {
		t.Errorf("Expected %d contracts, got %d", expectedContracts, stats.ContractsCreated)
	}

	// Genesis contract has 2 storage slots
	if stats.StorageSlotsCreated < 2 {
		t.Errorf("Expected at least 2 storage slots from genesis, got %d", stats.StorageSlotsCreated)
	}

	// State root should be non-zero
	if stats.StateRoot == (common.Hash{}) {
		t.Error("State root should not be zero")
	}

	// Verify genesis accounts are in the database by checking keys.
	// MPT-mode Generator delegates to the registered MPTGenerator and
	// closes its writer at the end; gen.DB() is nil. Reopen the DB
	// independently to inspect snapshot keys.
	gen.Close()
	db, err := pebble.New(dbPath, 64, 64, "verify/", true)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()

	// Check account 1 (EOA)
	addr1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	addrHash1 := crypto.Keccak256Hash(addr1[:])
	accountKey1 := append([]byte("a"), addrHash1[:]...)
	data1, err := db.Get(accountKey1)
	if err != nil {
		t.Errorf("Genesis EOA account not found in database: %v", err)
	}
	if len(data1) == 0 {
		t.Error("Genesis EOA account data is empty")
	}

	// Check account 2 (contract)
	addr2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	addrHash2 := crypto.Keccak256Hash(addr2[:])
	accountKey2 := append([]byte("a"), addrHash2[:]...)
	data2, err := db.Get(accountKey2)
	if err != nil {
		t.Errorf("Genesis contract account not found in database: %v", err)
	}
	if len(data2) == 0 {
		t.Error("Genesis contract account data is empty")
	}

	// Check storage slot
	slotKey := common.HexToHash("0x01")
	slotKeyHash := crypto.Keccak256Hash(slotKey[:])
	storageKey := append([]byte("o"), addrHash2[:]...)
	storageKey = append(storageKey, slotKeyHash[:]...)
	storageData, err := db.Get(storageKey)
	if err != nil {
		t.Errorf("Genesis storage slot not found in database: %v", err)
	}
	if len(storageData) == 0 {
		t.Error("Genesis storage slot data is empty")
	}

	// Check code
	codeHash := crypto.Keccak256Hash(genesisCode[addr2])
	codeKey := append([]byte("c"), codeHash[:]...)
	codeData, err := db.Get(codeKey)
	if err != nil {
		t.Errorf("Genesis code not found in database: %v", err)
	}
	if string(codeData) != string(genesisCode[addr2]) {
		t.Errorf("Genesis code mismatch: got %x, want %x", codeData, genesisCode[addr2])
	}
}

// TestGenesisAccountsIntegrationBinaryTrie tests that genesis alloc accounts
// are properly included in binary trie state generation.
func TestGenesisAccountsIntegrationBinaryTrie(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chaindata")

	genesisAccounts := map[common.Address]*types.StateAccount{
		common.HexToAddress("0x1111111111111111111111111111111111111111"): {
			Nonce:    0,
			Balance:  uint256.NewInt(1e18),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
		common.HexToAddress("0x2222222222222222222222222222222222222222"): {
			Nonce:    5,
			Balance:  uint256.NewInt(2e18),
			Root:     types.EmptyRootHash,
			CodeHash: crypto.Keccak256([]byte{0x60, 0x00}),
		},
	}

	genesisStorage := map[common.Address]map[common.Hash]common.Hash{
		common.HexToAddress("0x2222222222222222222222222222222222222222"): {
			common.HexToHash("0x01"): common.HexToHash("0xdeadbeef"),
			common.HexToHash("0x02"): common.HexToHash("0xcafe"),
		},
	}

	genesisCode := map[common.Address][]byte{
		common.HexToAddress("0x2222222222222222222222222222222222222222"): {0x60, 0x00},
	}

	plan := mustPlan(t, 512<<10)
	config := Config{
		DBPath:          dbPath,
		AutoFill:        plan,
		Seed:            12345,
		Verbose:         false,
		TrieMode:        TrieModeBinary,
		GenesisAccounts: genesisAccounts,
		GenesisStorage:  genesisStorage,
		GenesisCode:     genesisCode,
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

	// Verify stats: 1 genesis EOA + plan EOAs / 1 genesis contract + plan contracts.
	expectedAccounts := 1 + plan.NumEOAs
	expectedContracts := 1 + plan.NumContracts

	if stats.AccountsCreated != expectedAccounts {
		t.Errorf("Expected %d accounts, got %d", expectedAccounts, stats.AccountsCreated)
	}
	if stats.ContractsCreated != expectedContracts {
		t.Errorf("Expected %d contracts, got %d", expectedContracts, stats.ContractsCreated)
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Error("State root should not be zero")
	}
	// Genesis contract has 2 storage slots
	if stats.StorageSlotsCreated < 2 {
		t.Errorf("Expected at least 2 storage slots from genesis, got %d", stats.StorageSlotsCreated)
	}

	// In binary trie mode, flat state is stored as stem blobs under "vX" prefix,
	// NOT as MPT-style "a"/"o" snapshot entries.
	db := gen.DB()

	// Verify genesis EOA exists in stem blobs
	addr1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	var zeroKey [hashSize]byte
	stem1 := bintrie.GetBinaryTreeKey(addr1, zeroKey[:])
	stemBlobKey1 := append([]byte("vX"), stem1[:stemSize]...)
	data1, err := db.Get(stemBlobKey1)
	if err != nil {
		t.Errorf("Genesis EOA stem blob not found in database: %v", err)
	}
	if len(data1) < hashSize {
		t.Error("Genesis EOA stem blob is too short")
	}

	// Verify genesis contract exists in stem blobs
	addr2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	stem2 := bintrie.GetBinaryTreeKey(addr2, zeroKey[:])
	stemBlobKey2 := append([]byte("vX"), stem2[:stemSize]...)
	data2, err := db.Get(stemBlobKey2)
	if err != nil {
		t.Errorf("Genesis contract stem blob not found in database: %v", err)
	}
	if len(data2) < hashSize {
		t.Error("Genesis contract stem blob is too short")
	}

	// Verify code (still under "c" prefix)
	codeHash := crypto.Keccak256Hash(genesisCode[addr2])
	codeData, err := db.Get(append([]byte("c"), codeHash[:]...))
	if err != nil {
		t.Errorf("Genesis code not found: %v", err)
	}
	if string(codeData) != string(genesisCode[addr2]) {
		t.Errorf("Genesis code mismatch: got %x, want %x", codeData, genesisCode[addr2])
	}

	// Verify root differs from MPT with same config
	mptDir := t.TempDir()
	mptConfig := config
	mptConfig.DBPath = filepath.Join(mptDir, "chaindata")
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
		t.Error("Binary trie and MPT should produce different roots for same genesis config")
	}
}

// TestGenesisAccountNoCollision verifies that generated accounts don't
// collide with genesis accounts (extremely unlikely but we check anyway).
func TestGenesisAccountNoCollision(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chaindata")

	// Create a genesis account
	genesisAccounts := map[common.Address]*types.StateAccount{
		common.HexToAddress("0x1111111111111111111111111111111111111111"): {
			Nonce:    0,
			Balance:  uint256.NewInt(1e18),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
	}

	plan := mustPlan(t, 2<<20)
	config := Config{
		DBPath:          dbPath,
		AutoFill:        plan,
		Seed:            99999,
		Verbose:         false,
		GenesisAccounts: genesisAccounts,
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

	// Should have 1 genesis EOA + plan.NumEOAs synthetic + plan.NumContracts.
	if stats.AccountsCreated != 1+plan.NumEOAs {
		t.Errorf("Expected %d accounts (1 genesis + %d synthetic), got %d",
			1+plan.NumEOAs, plan.NumEOAs, stats.AccountsCreated)
	}
	if stats.ContractsCreated != plan.NumContracts {
		t.Errorf("Expected %d contracts, got %d", plan.NumContracts, stats.ContractsCreated)
	}
}

// TestReproducibilityWithGenesis tests that state generation with genesis
// is reproducible given the same seed.
func TestReproducibilityWithGenesis(t *testing.T) {
	genesisAccounts := map[common.Address]*types.StateAccount{
		common.HexToAddress("0xaaaa"): {
			Nonce:    1,
			Balance:  uint256.NewInt(5e18),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
	}

	var roots []common.Hash

	for i := 0; i < 3; i++ {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "chaindata")

		plan := mustPlan(t, 2<<20)
		config := Config{
			DBPath:          dbPath,
			AutoFill:        plan,
			Seed:            42,
			Verbose:         false,
			GenesisAccounts: genesisAccounts,
		}

		gen, err := New(config)
		if err != nil {
			t.Fatalf("Failed to create generator: %v", err)
		}

		stats, err := gen.Generate()
		gen.Close()

		if err != nil {
			t.Fatalf("Failed to generate state: %v", err)
		}

		roots = append(roots, stats.StateRoot)
	}

	// All runs should produce identical state roots
	for i := 1; i < len(roots); i++ {
		if roots[i] != roots[0] {
			t.Errorf("State root mismatch on run %d: got %s, want %s", i+1, roots[i].Hex(), roots[0].Hex())
		}
	}
}

// TestEmptyGenesisAccounts tests that the generator works correctly
// when no genesis accounts are provided.
func TestEmptyGenesisAccounts(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chaindata")

	plan := mustPlan(t, 512<<10)
	config := Config{
		DBPath:   dbPath,
		AutoFill: plan,
		Seed:     123,
		Verbose:  false,
		// No GenesisAccounts
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

	if stats.AccountsCreated != plan.NumEOAs {
		t.Errorf("Expected %d accounts, got %d", plan.NumEOAs, stats.AccountsCreated)
	}
	if stats.ContractsCreated != plan.NumContracts {
		t.Errorf("Expected %d contracts, got %d", plan.NumContracts, stats.ContractsCreated)
	}
}

// TestLargeGenesisAlloc tests generation with many genesis accounts.
func TestLargeGenesisAlloc(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large genesis alloc test in short mode")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chaindata")

	// Create 1000 genesis accounts
	genesisAccounts := make(map[common.Address]*types.StateAccount)
	for i := 0; i < 1000; i++ {
		addr := common.BigToAddress(big.NewInt(int64(i + 1000)))
		genesisAccounts[addr] = &types.StateAccount{
			Nonce:    uint64(i),
			Balance:  uint256.NewInt(uint64(i) * 1e15),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}
	}

	plan := mustPlan(t, 2<<20)
	config := Config{
		DBPath:          dbPath,
		AutoFill:        plan,
		Seed:            777,
		Verbose:         false,
		GenesisAccounts: genesisAccounts,
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

	// 1000 genesis EOAs + plan.NumEOAs synthetic.
	if stats.AccountsCreated != 1000+plan.NumEOAs {
		t.Errorf("Expected %d accounts (1000 genesis + %d synthetic), got %d",
			1000+plan.NumEOAs, plan.NumEOAs, stats.AccountsCreated)
	}
	if stats.ContractsCreated != plan.NumContracts {
		t.Errorf("Expected %d contracts, got %d", plan.NumContracts, stats.ContractsCreated)
	}

	// Cleanup
	os.RemoveAll(dbPath)
}
