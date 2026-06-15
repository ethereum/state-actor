package geth

import (
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
)

// Compile-time assertion that *Writer satisfies the generator.Writer interface.
// If this line fails to compile, geth.Writer is missing a method or has a
// signature mismatch with the interface.
var _ generator.Writer = (*Writer)(nil)

func TestWriterBasic(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "geth-writer-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	w, err := NewWriter(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create geth writer: %v", err)
	}
	defer w.Close()

	addr := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")
	acc := &types.StateAccount{
		Nonce:    42,
		Balance:  uint256.NewInt(1000000000000000000), // 1 ETH
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}

	if err := w.WriteAccount(addr, crypto.Keccak256Hash(addr[:]), acc, 0); err != nil {
		t.Fatalf("Failed to write account: %v", err)
	}

	slot := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")
	value := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000002a")

	if err := w.WriteStorage(addr, crypto.Keccak256Hash(addr[:]), slot, crypto.Keccak256Hash(slot[:]), value); err != nil {
		t.Fatalf("Failed to write storage: %v", err)
	}

	if err := w.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}

	stats := w.Stats()
	if stats.AccountBytes == 0 {
		t.Error("Expected non-zero account bytes")
	}
	if stats.StorageBytes == 0 {
		t.Error("Expected non-zero storage bytes")
	}

	t.Logf("geth writer stats: accounts=%d, storage=%d, code=%d",
		stats.AccountBytes, stats.StorageBytes, stats.CodeBytes)
}

// TestSetStateRootMPT verifies the SetStateRoot writer-side metadata writes
// for the MPT (non-bintrie) case: PathDB metadata at raw keys, no v-prefix
// duplication, and DatabaseVersion present at the raw key.
//
// Without these, geth either treats the DB as uninitialized (missing
// DatabaseVersion) or triggers a full snapshot regeneration on first open
// (missing/mismatched SnapshotGenerator).
func TestSetStateRootMPT(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "geth-writer-setroot-mpt-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	w, err := NewWriter(tmpDir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	root := common.HexToHash("0xabababababababababababababababababababababababababababababababab")
	if err := w.SetStateRoot(root, false /* binaryTrie */, false /* archive */); err != nil {
		t.Fatalf("SetStateRoot: %v", err)
	}

	db := w.DB()
	for _, key := range []string{"SnapshotRoot", "SnapshotGenerator"} {
		val, _ := db.Get([]byte(key))
		if len(val) == 0 {
			t.Errorf("key %q missing at raw position in MPT mode", key)
		}
		if prefixed, _ := db.Get(append([]byte("v"), []byte(key)...)); len(prefixed) != 0 {
			t.Errorf("key %q unexpectedly present under v-prefix in MPT mode: %x", key, prefixed)
		}
	}

	v := rawdb.ReadDatabaseVersion(db)
	if v == nil {
		t.Fatal("DatabaseVersion missing — geth will overwrite the DB on startup")
	}
	if *v != pathdbSchemaVersion {
		t.Errorf("DatabaseVersion = %d, want %d", *v, pathdbSchemaVersion)
	}
}

// TestSetStateRootBinaryTrie verifies the SetStateRoot writer-side metadata
// writes for the bintrie case: PathDB metadata under the "v" prefix (so
// pathdb's wrapped diskdb finds it), DatabaseVersion at the raw key (so
// geth's pre-pathdb startup gate finds it), and no raw-key leakage of the
// pathdb-namespaced entries.
func TestSetStateRootBinaryTrie(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "geth-writer-setroot-bintrie-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	w, err := NewWriter(tmpDir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	root := common.HexToHash("0xcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd")
	if err := w.SetStateRoot(root, true /* binaryTrie */, false /* archive */); err != nil {
		t.Fatalf("SetStateRoot: %v", err)
	}

	db := w.DB()
	for _, key := range []string{"SnapshotRoot", "SnapshotGenerator"} {
		prefixed, _ := db.Get(append([]byte("v"), []byte(key)...))
		if len(prefixed) == 0 {
			t.Errorf("key %q missing under v-prefix in bintrie mode", key)
		}
		if raw, _ := db.Get([]byte(key)); len(raw) != 0 {
			t.Errorf("key %q unexpectedly present at raw position in bintrie mode: %x", key, raw)
		}
	}

	v := rawdb.ReadDatabaseVersion(db)
	if v == nil {
		t.Fatal("DatabaseVersion missing — geth will overwrite the DB on startup")
	}
	if *v != pathdbSchemaVersion {
		t.Errorf("DatabaseVersion = %d, want %d", *v, pathdbSchemaVersion)
	}
	// DatabaseVersion lives at the raw key — never under v-prefix.
	if prefixed, _ := db.Get(append([]byte("v"), []byte("DatabaseVersion")...)); len(prefixed) != 0 {
		t.Errorf("DatabaseVersion unexpectedly present under v-prefix: %x", prefixed)
	}
}
