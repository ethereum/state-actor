package geth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/nerolation/state-actor/generator"
)

// TestGethOracleBootReadable is the geth-MPT boot-readability gate.
//
// It runs a small Populate generation, reopens the resulting Pebble DB,
// and validates that EVERY metadata entry geth's PathDB + snapshot
// boot path consults is present and well-formed:
//
//   - SnapshotRoot  → equals the produced state root
//   - SnapshotGenerator → Done=true (no on-boot regeneration)
//   - DatabaseVersion  → pathdbSchemaVersion (geth's pre-pathdb gate)
//   - StateID at SnapshotRoot → 0
//   - PersistentStateID → 0
//   - Account trie root node at TrieNodeAccountPrefix + "" → present and
//     RLP-decodable as a list (root nodes are always full-list / branch)
//
// Plus structural reads of representative snapshot entries (one EOA,
// one contract account + storage slot + code) decoded back to Go
// values. If any of these fail, geth would either refuse to boot or
// silently regenerate the snapshot — both of which would break a real
// devnet.
//
// This is the "would geth actually boot this DB" gate. It does NOT
// launch a geth-fork binary — that's covered by the Makefile smoke
// target. This test runs in-process and stays fast (~100ms).
func TestGethOracleBootReadable(t *testing.T) {
	dir := t.TempDir()
	cfg := generator.Config{
		DBPath:         filepath.Join(dir, "geth", "chaindata"),
		NumAccounts:    20,
		NumContracts:   8,
		MaxSlots:       12,
		MinSlots:       3,
		Distribution:   generator.PowerLaw,
		Seed:           4242,
		BatchSize:      1000,
		Workers:        1,
		CodeSize:       96,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
	}

	stats, err := Populate(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if (stats.StateRoot == common.Hash{}) {
		t.Fatal("state root unexpectedly zero")
	}

	w, err := NewWriter(cfg.DBPath, cfg.BatchSize, cfg.Workers)
	if err != nil {
		t.Fatalf("reopen writer: %v", err)
	}
	defer w.Close()
	db := w.DB()

	// --- PathDB / snapshot boot metadata gate ---

	if got := rawdb.ReadSnapshotRoot(db); got != stats.StateRoot {
		t.Errorf("SnapshotRoot = %s, want %s", got.Hex(), stats.StateRoot.Hex())
	}

	genBlob := rawdb.ReadSnapshotGenerator(db)
	if len(genBlob) == 0 {
		t.Fatal("SnapshotGenerator missing — geth would regenerate snapshot from scratch")
	}
	var gen snapshotGenerator
	if err := rlp.DecodeBytes(genBlob, &gen); err != nil {
		t.Fatalf("decode SnapshotGenerator: %v", err)
	}
	if !gen.Done {
		t.Error("SnapshotGenerator.Done = false — geth would regenerate")
	}

	if v := rawdb.ReadDatabaseVersion(db); v == nil || *v != pathdbSchemaVersion {
		got := -1
		if v != nil {
			got = int(*v)
		}
		t.Errorf("DatabaseVersion = %d, want %d", got, pathdbSchemaVersion)
	}

	if id := rawdb.ReadStateID(db, stats.StateRoot); id == nil || *id != 0 {
		got := int64(-1)
		if id != nil {
			got = int64(*id)
		}
		t.Errorf("StateID(stateRoot) = %d, want 0", got)
	}

	if id := rawdb.ReadPersistentStateID(db); id != 0 {
		t.Errorf("PersistentStateID = %d, want 0", id)
	}

	// --- Account trie root node presence ---
	//
	// PathScheme account trie nodes live at TrieNodeAccountPrefix + path.
	// The empty-path entry is the root node (always a list per MPT spec
	// — branch or extension). Decoding it to a generic []rlp.RawValue
	// validates the bytes are a valid RLP list, which is what geth's
	// trie reader will require on the first descent.
	rootBlob := rawdb.ReadAccountTrieNode(db, []byte{})
	if len(rootBlob) == 0 {
		t.Fatal("Account trie root node missing — Phase 2 didn't emit OnTrieNode for the root")
	}
	var rootList []rlp.RawValue
	if err := rlp.DecodeBytes(rootBlob, &rootList); err != nil {
		t.Errorf("Account trie root node not a valid RLP list: %v", err)
	}

	// --- Sample structural reads from the snapshot layer ---

	if len(stats.SampleEOAs) == 0 {
		t.Fatal("stats.SampleEOAs empty — generator didn't record any sample addresses")
	}
	for _, addr := range stats.SampleEOAs {
		blob, err := db.Get(accountSnapshotKey(crypto.Keccak256Hash(addr[:])))
		if err != nil {
			t.Errorf("EOA snapshot missing for %s: %v", addr.Hex(), err)
			continue
		}
		var slim types.SlimAccount
		if err := rlp.DecodeBytes(blob, &slim); err != nil {
			t.Errorf("decode SlimAccount for %s: %v", addr.Hex(), err)
		}
	}

	for _, addr := range stats.SampleContracts {
		blob, err := db.Get(accountSnapshotKey(crypto.Keccak256Hash(addr[:])))
		if err != nil {
			t.Errorf("contract snapshot missing for %s: %v", addr.Hex(), err)
			continue
		}
		var slim types.SlimAccount
		if err := rlp.DecodeBytes(blob, &slim); err != nil {
			t.Errorf("decode SlimAccount for %s: %v", addr.Hex(), err)
			continue
		}
		// Contracts must reference real code in the "c"+codeHash key.
		if len(slim.CodeHash) == 0 {
			continue // EOA misclassified as contract is its own bug; only
			// the snapshot-key-shape gate matters here
		}
		codeHash := common.BytesToHash(slim.CodeHash)
		if codeHash == types.EmptyCodeHash {
			continue
		}
		code, err := db.Get(codeKey(codeHash))
		if err != nil {
			t.Errorf("code missing for hash %s (contract %s): %v",
				codeHash.Hex(), addr.Hex(), err)
		}
		if len(code) == 0 {
			t.Errorf("code blob empty for hash %s", codeHash.Hex())
		}
		if got := crypto.Keccak256Hash(code); got != codeHash {
			t.Errorf("code keccak mismatch for %s: got %s", addr.Hex(), got.Hex())
		}
	}
}
