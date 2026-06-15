package geth

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/genesis"
)

// readSnapshotGeneratorEntry reads and decodes the SnapshotGenerator blob,
// optionally under a key prefix (used for binary trie mode's "v" namespace).
func readSnapshotGeneratorEntry(t *testing.T, db ethdb.KeyValueReader, prefix []byte) snapshotGenerator {
	t.Helper()
	// rawdb.ReadSnapshotGenerator uses a fixed key, so for the prefixed case
	// we read directly from the DB.
	var blob []byte
	if len(prefix) == 0 {
		blob = rawdb.ReadSnapshotGenerator(db)
	} else {
		key := append(append([]byte{}, prefix...), []byte("SnapshotGenerator")...)
		var err error
		blob, err = db.Get(key)
		if err != nil {
			t.Fatalf("SnapshotGenerator missing under prefix %q: %v", prefix, err)
		}
	}
	if len(blob) == 0 {
		t.Fatal("SnapshotGenerator blob is empty")
	}
	var gen snapshotGenerator
	if err := rlp.DecodeBytes(blob, &gen); err != nil {
		t.Fatalf("decode SnapshotGenerator: %v", err)
	}
	return gen
}

// sampleGenesis creates a minimal genesis configuration for testing.
// Duplicated from genesis/genesis_test.go's helper because Go test fixtures
// don't share across packages; the body here mirrors the reference fixture
// so behavior remains comparable.
func sampleGenesis() *genesis.Genesis {
	chainConfig := &params.ChainConfig{
		ChainID:                 big.NewInt(1337),
		HomesteadBlock:          big.NewInt(0),
		EIP150Block:             big.NewInt(0),
		EIP155Block:             big.NewInt(0),
		EIP158Block:             big.NewInt(0),
		ByzantiumBlock:          big.NewInt(0),
		ConstantinopleBlock:     big.NewInt(0),
		PetersburgBlock:         big.NewInt(0),
		IstanbulBlock:           big.NewInt(0),
		MuirGlacierBlock:        big.NewInt(0),
		BerlinBlock:             big.NewInt(0),
		LondonBlock:             big.NewInt(0),
		MergeNetsplitBlock:      big.NewInt(0),
		ShanghaiTime:            new(uint64),
		CancunTime:              new(uint64),
		TerminalTotalDifficulty: big.NewInt(0),
	}

	return &genesis.Genesis{
		Config:     chainConfig,
		Nonce:      0,
		Timestamp:  0,
		ExtraData:  []byte("test genesis"),
		GasLimit:   hexutil.Uint64(30000000),
		Difficulty: (*hexutil.Big)(big.NewInt(0)),
		Alloc: genesis.GenesisAlloc{
			common.HexToAddress("0x1111111111111111111111111111111111111111"): {
				Balance: (*hexutil.Big)(big.NewInt(1e18)),
				Nonce:   0,
			},
			common.HexToAddress("0x2222222222222222222222222222222222222222"): {
				Balance: (*hexutil.Big)(big.NewInt(2e18)),
				Code:    hexutil.Bytes{0x60, 0x00, 0x60, 0x00, 0xf3}, // PUSH1 0 PUSH1 0 RETURN
				Storage: map[common.Hash]common.Hash{
					common.HexToHash("0x01"): common.HexToHash("0xdeadbeef"),
				},
			},
		},
	}
}

func TestWriteGenesisBlock(t *testing.T) {
	// Use rawdb.NewMemoryDatabase which implements full ethdb.Database interface
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	gen := sampleGenesis()

	// Use a deterministic state root for testing
	stateRoot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	block, err := WriteGenesisBlock(db, gen, stateRoot, false /* binaryTrie */, false /* archive */, "")
	if err != nil {
		t.Fatalf("Failed to write genesis block: %v", err)
	}

	// Verify block properties
	if block.NumberU64() != 0 {
		t.Errorf("Genesis block number should be 0, got %d", block.NumberU64())
	}
	if block.Root() != stateRoot {
		t.Errorf("State root mismatch: got %s, want %s", block.Root().Hex(), stateRoot.Hex())
	}

	// Verify database entries
	// 1. Canonical hash
	canonicalHash := rawdb.ReadCanonicalHash(db, 0)
	if canonicalHash != block.Hash() {
		t.Errorf("Canonical hash mismatch: got %s, want %s", canonicalHash.Hex(), block.Hash().Hex())
	}

	// 2. Head block hash
	headBlockHash := rawdb.ReadHeadBlockHash(db)
	if headBlockHash != block.Hash() {
		t.Errorf("Head block hash mismatch: got %s, want %s", headBlockHash.Hex(), block.Hash().Hex())
	}

	// 3. Head header hash
	headHeaderHash := rawdb.ReadHeadHeaderHash(db)
	if headHeaderHash != block.Hash() {
		t.Errorf("Head header hash mismatch: got %s, want %s", headHeaderHash.Hex(), block.Hash().Hex())
	}

	// 4. Chain config
	chainConfig := rawdb.ReadChainConfig(db, block.Hash())
	if chainConfig == nil {
		t.Error("Chain config not found in database")
	} else {
		if chainConfig.ChainID.Cmp(big.NewInt(1337)) != 0 {
			t.Errorf("Chain ID mismatch: got %s, want 1337", chainConfig.ChainID)
		}
		if chainConfig.EnableVerkleAtGenesis {
			t.Error("EnableVerkleAtGenesis should be false when binaryTrie=false")
		}
	}

	// 5. Block should be retrievable
	storedBlock := rawdb.ReadBlock(db, block.Hash(), 0)
	if storedBlock == nil {
		t.Error("Genesis block not found in database")
	} else if storedBlock.Hash() != block.Hash() {
		t.Errorf("Retrieved block hash mismatch: got %s, want %s", storedBlock.Hash().Hex(), block.Hash().Hex())
	}
}

func TestWriteGenesisBlockWithShanghai(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	gen := sampleGenesis()
	// Enable Shanghai at genesis
	zero := uint64(0)
	gen.Config.ShanghaiTime = &zero

	stateRoot := common.HexToHash("0xabcd")
	block, err := WriteGenesisBlock(db, gen, stateRoot, false /* binaryTrie */, false /* archive */, "")
	if err != nil {
		t.Fatalf("Failed to write genesis block: %v", err)
	}

	// Shanghai blocks should have withdrawals hash
	if block.Header().WithdrawalsHash == nil {
		t.Error("Shanghai genesis should have withdrawals hash")
	}
	if *block.Header().WithdrawalsHash != types.EmptyWithdrawalsHash {
		t.Error("Genesis withdrawals hash should be empty withdrawals hash")
	}
}

func TestWriteGenesisBlockWithCancun(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	gen := sampleGenesis()
	// Enable Cancun at genesis
	zero := uint64(0)
	gen.Config.ShanghaiTime = &zero
	gen.Config.CancunTime = &zero

	stateRoot := common.HexToHash("0xabcd")
	block, err := WriteGenesisBlock(db, gen, stateRoot, false /* binaryTrie */, false /* archive */, "")
	if err != nil {
		t.Fatalf("Failed to write genesis block: %v", err)
	}

	header := block.Header()

	// Cancun blocks should have blob gas fields
	if header.ExcessBlobGas == nil {
		t.Error("Cancun genesis should have excess blob gas")
	}
	if header.BlobGasUsed == nil {
		t.Error("Cancun genesis should have blob gas used")
	}
	if header.ParentBeaconRoot == nil {
		t.Error("Cancun genesis should have parent beacon root")
	}
}

func TestWriteGenesisBlockBinaryTrie(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	gen := sampleGenesis()
	stateRoot := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	block, err := WriteGenesisBlock(db, gen, stateRoot, true /* binaryTrie */, false /* archive */, "")
	if err != nil {
		t.Fatalf("Failed to write genesis block: %v", err)
	}

	// Verify chain config was persisted with EnableVerkleAtGenesis
	chainConfig := rawdb.ReadChainConfig(db, block.Hash())
	if chainConfig == nil {
		t.Fatal("Chain config not found in database")
	}
	if !chainConfig.EnableVerkleAtGenesis {
		t.Error("EnableVerkleAtGenesis should be true for binary trie mode")
	}

	// Verify block is readable
	storedBlock := rawdb.ReadBlock(db, block.Hash(), 0)
	if storedBlock == nil {
		t.Error("Genesis block not found in database")
	}
	if storedBlock.Root() != stateRoot {
		t.Errorf("State root mismatch: got %s, want %s", storedBlock.Root().Hex(), stateRoot.Hex())
	}
}

// TestWriteGenesisBlockSnapshotGeneratorMPT verifies that WriteGenesisBlock
// persists a SnapshotGenerator blob with Done=true under the top-level
// (no-prefix) namespace when binaryTrie=false.
//
// Without this entry, geth's pathdb.loadGenerator returns nil, which causes
// setStateGenerator to construct a fresh empty-marker generator. In MPT mode
// (noBuild=false) this triggers a full snapshot regeneration from scratch.
func TestWriteGenesisBlockSnapshotGeneratorMPT(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	stateRoot := common.HexToHash("0xfeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface")
	if _, err := WriteGenesisBlock(db, sampleGenesis(), stateRoot, false /* binaryTrie */, false /* archive */, ""); err != nil {
		t.Fatalf("WriteGenesisBlock: %v", err)
	}

	gen := readSnapshotGeneratorEntry(t, db, nil)
	if !gen.Done {
		t.Errorf("SnapshotGenerator.Done = false, want true")
	}
	// Marker is intentionally not asserted: RLP-decoded []byte fields lose
	// nil-ness and round-trip as empty slices. pathdb's setStateGenerator
	// short-circuits on Done==true before inspecting Marker, so its value
	// is immaterial to the regeneration-prevention behavior we care about.
}

// TestWriteGenesisBlockSnapshotGeneratorBinaryTrie verifies that the
// SnapshotGenerator blob is written under the "v" (rawdb.VerklePrefix)
// namespace in binary trie mode, where pathdb opens the diskdb wrapped
// by rawdb.NewTable(diskdb, "v").
//
// We additionally assert the blob is NOT present at the top level — leaking
// it there would be harmless but indicates wiring drift.
func TestWriteGenesisBlockSnapshotGeneratorBinaryTrie(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	stateRoot := common.HexToHash("0xc0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff")
	if _, err := WriteGenesisBlock(db, sampleGenesis(), stateRoot, true /* binaryTrie */, false /* archive */, ""); err != nil {
		t.Fatalf("WriteGenesisBlock: %v", err)
	}

	gen := readSnapshotGeneratorEntry(t, db, []byte("v"))
	if !gen.Done {
		t.Errorf("SnapshotGenerator.Done = false under v-prefix, want true")
	}

	// Top-level key must be empty in binary trie mode.
	if blob := rawdb.ReadSnapshotGenerator(db); len(blob) != 0 {
		t.Errorf("top-level SnapshotGenerator unexpectedly written in binary trie mode: %x", blob)
	}
}

func TestWriteGenesisBlockBinaryTrieNoMutation(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	gen := sampleGenesis()
	origConfig := gen.Config
	origVerkle := gen.Config.EnableVerkleAtGenesis

	stateRoot := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	_, err := WriteGenesisBlock(db, gen, stateRoot, true /* binaryTrie */, false /* archive */, "")
	if err != nil {
		t.Fatalf("Failed to write genesis block: %v", err)
	}

	// The caller's Genesis.Config pointer must not have been replaced
	if gen.Config != origConfig {
		t.Error("WriteGenesisBlock replaced caller's genesis.Config pointer")
	}
	// The original ChainConfig must not have been mutated
	if gen.Config.EnableVerkleAtGenesis != origVerkle {
		t.Errorf("WriteGenesisBlock mutated caller's EnableVerkleAtGenesis: got %v, want %v",
			gen.Config.EnableVerkleAtGenesis, origVerkle)
	}
}

// TestWriteGenesisBlockDatabaseVersionMPT pins the rawdb DatabaseVersion gate
// that geth uses at startup to decide "is this DB initialized?". A nil value
// causes geth --dev to silently overwrite the DB with a fresh dev genesis,
// discarding everything state-actor wrote.
//
// DatabaseVersion is read before pathdb is constructed, so it must live at
// the raw key (NOT under the "v" prefix) regardless of trie mode.
func TestWriteGenesisBlockDatabaseVersionMPT(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	stateRoot := common.HexToHash("0x1212121212121212121212121212121212121212121212121212121212121212")
	if _, err := WriteGenesisBlock(db, sampleGenesis(), stateRoot, false /* binaryTrie */, false /* archive */, ""); err != nil {
		t.Fatalf("WriteGenesisBlock: %v", err)
	}
	assertDatabaseVersionAtRawKey(t, db)
}

// TestWriteGenesisBlockDatabaseVersionBinaryTrie is the bintrie counterpart:
// even in bintrie mode, DatabaseVersion stays at the raw key — never under
// "v" — because geth reads it before pathdb wraps the diskdb.
func TestWriteGenesisBlockDatabaseVersionBinaryTrie(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	stateRoot := common.HexToHash("0x3434343434343434343434343434343434343434343434343434343434343434")
	if _, err := WriteGenesisBlock(db, sampleGenesis(), stateRoot, true /* binaryTrie */, false /* archive */, ""); err != nil {
		t.Fatalf("WriteGenesisBlock: %v", err)
	}
	assertDatabaseVersionAtRawKey(t, db)

	// And it must NOT be present under the v-prefix — leaking it there would
	// confuse a future reader and mask a missing raw-key write.
	prefixed, err := db.Get(append([]byte("v"), []byte("DatabaseVersion")...))
	if err == nil && len(prefixed) != 0 {
		t.Errorf("DatabaseVersion unexpectedly present under v-prefix: %x", prefixed)
	}
}

// TestSnapshotGeneratorIsBintrieRoundTripMPT pins IsBintrie=false in MPT
// mode. pathdb/journal.go discards the generator unless IsBintrie matches
// the opening database's mode, so a stray true here would trigger a full
// snapshot regeneration.
func TestSnapshotGeneratorIsBintrieRoundTripMPT(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	stateRoot := common.HexToHash("0x5656565656565656565656565656565656565656565656565656565656565656")
	if _, err := WriteGenesisBlock(db, sampleGenesis(), stateRoot, false /* binaryTrie */, false /* archive */, ""); err != nil {
		t.Fatalf("WriteGenesisBlock: %v", err)
	}
	gen := readSnapshotGeneratorEntry(t, db, nil)
	if !gen.Done {
		t.Error("SnapshotGenerator.Done = false, want true")
	}
	if gen.IsBintrie {
		t.Error("SnapshotGenerator.IsBintrie = true in MPT mode, want false")
	}
}

// TestSnapshotGeneratorIsBintrieRoundTripBinaryTrie pins IsBintrie=true in
// bintrie mode and verifies it lives under the "v" prefix. The dual signal
// (prefix + struct field) is required because pathdb checks both.
func TestSnapshotGeneratorIsBintrieRoundTripBinaryTrie(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()

	stateRoot := common.HexToHash("0x7878787878787878787878787878787878787878787878787878787878787878")
	if _, err := WriteGenesisBlock(db, sampleGenesis(), stateRoot, true /* binaryTrie */, false /* archive */, ""); err != nil {
		t.Fatalf("WriteGenesisBlock: %v", err)
	}
	gen := readSnapshotGeneratorEntry(t, db, []byte("v"))
	if !gen.Done {
		t.Error("SnapshotGenerator.Done = false under v-prefix, want true")
	}
	if !gen.IsBintrie {
		t.Error("SnapshotGenerator.IsBintrie = false in bintrie mode, want true")
	}
}

// TestWritePathDBMetadataPrefixing verifies the full key layout produced by
// WritePathDBMetadata in isolation, without depending on WriteGenesisBlock.
func TestWritePathDBMetadataPrefixing(t *testing.T) {
	stateRoot := common.HexToHash("0x9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a")

	for _, tc := range []struct {
		name       string
		binaryTrie bool
		// pathdbKeys are read prefixed in bintrie mode, raw in MPT.
		pathdbKeys []string
	}{
		{name: "MPT", binaryTrie: false, pathdbKeys: []string{"SnapshotRoot", "SnapshotGenerator"}},
		{name: "BinaryTrie", binaryTrie: true, pathdbKeys: []string{"SnapshotRoot", "SnapshotGenerator"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			defer db.Close()

			if err := WritePathDBMetadata(db, stateRoot, tc.binaryTrie, false /* archive */); err != nil {
				t.Fatalf("WritePathDBMetadata: %v", err)
			}

			assertDatabaseVersionAtRawKey(t, db)

			for _, key := range tc.pathdbKeys {
				rawVal, _ := db.Get([]byte(key))
				prefixedVal, _ := db.Get(append([]byte("v"), []byte(key)...))

				if tc.binaryTrie {
					if len(rawVal) != 0 {
						t.Errorf("key %q present at raw position in bintrie mode: %x", key, rawVal)
					}
					if len(prefixedVal) == 0 {
						t.Errorf("key %q missing under v-prefix in bintrie mode", key)
					}
				} else {
					if len(rawVal) == 0 {
						t.Errorf("key %q missing at raw position in MPT mode", key)
					}
					if len(prefixedVal) != 0 {
						t.Errorf("key %q unexpectedly present under v-prefix in MPT mode: %x", key, prefixedVal)
					}
				}
			}
		})
	}
}

func assertDatabaseVersionAtRawKey(t *testing.T, db ethdb.KeyValueReader) {
	t.Helper()
	v := rawdb.ReadDatabaseVersion(db)
	if v == nil {
		t.Fatal("DatabaseVersion missing — geth will treat the DB as uninitialized and overwrite it")
	}
	if *v != pathdbSchemaVersion {
		t.Errorf("DatabaseVersion = %d, want %d", *v, pathdbSchemaVersion)
	}
}
