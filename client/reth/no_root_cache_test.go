//go:build cgo_reth

package reth

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/autofill"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestNoRootCacheRows guards the invariant that state-actor's AccountsTrie
// and StoragesTrie writers never persist the root branch — mirroring reth's
// own writer guards in provider.rs (`if !key.is_empty()`) and
// trie_cursor.rs (`.filter(|(n, _)| !n.is_empty())`). Without these guards
// (regressed in any of the 3 emit callbacks in run_cgo.go,
// contracts_writer_cgo.go, spec_storage_streaming_cgo.go), reth's proof_v2
// cursor would decode the 33-zero-byte AccountsTrie key (or all-zero
// packed StoragesTrie SubKey) as cached_path == empty Nibbles and panic.
func TestNoRootCacheRows(t *testing.T) {
	tmp := t.TempDir()

	// Coverage knobs to exercise all 3 emit sites:
	//   - AutoFill plan → batched WriteEOAs + WriteContracts → AccountsTrie
	//     branching (run_cgo emit) + per-contract storage (contracts_writer emit).
	//   - PreAlloc with non-empty Storage → spec_storage_streaming emit.
	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	specStorage := map[common.Hash]common.Hash{}
	for i := byte(0); i < 8; i++ {
		var k, v common.Hash
		k[31] = i
		v[31] = i + 1
		specStorage[k] = v
	}
	cfg := generator.Config{
		DBPath:   tmp,
		AutoFill: plan,
		Seed:     12345,
		PreAlloc: []templates.PreAllocEntity{{
			Address: common.HexToAddress("0x00000000000000000000000000000000deadbeef"),
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(42),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash[:],
			},
			Storage: func(yield func(common.Hash, common.Hash) bool) {
				for k, v := range specStorage {
					if !yield(k, v) {
						return
					}
				}
			},
		}},
	}
	if _, err := RunCgo(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	env, err := mdbx.NewEnv()
	if err != nil {
		t.Fatalf("mdbx.NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetOption(mdbx.OptMaxDB, 64); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	if err := env.Open(filepath.Join(tmp, "db"), mdbx.Readonly, 0o644); err != nil {
		t.Fatalf("env.Open: %v", err)
	}

	if err := env.View(func(txn *mdbx.Txn) error {
		checkAccountsTrieNoRoot(t, txn)
		checkStoragesTrieNoRoot(t, txn)
		return nil
	}); err != nil {
		t.Fatalf("env.View: %v", err)
	}
}

// checkAccountsTrieNoRoot asserts:
//
//  1. txn.Get(AccountsTrie, [33]byte{}) returns mdbx.NotFound (no explicit
//     root-cache row at the 33-zero-byte packed key).
//  2. cursor.First() returns either no rows (empty table) or a key with
//     nibble count > 0 — i.e., for the 33-byte packed encoding, the last
//     byte (count suffix) is non-zero.
func checkAccountsTrieNoRoot(t *testing.T, txn *mdbx.Txn) {
	t.Helper()
	dbi, err := txn.OpenDBI("AccountsTrie", 0, nil, nil)
	if err != nil {
		t.Fatalf("OpenDBI AccountsTrie: %v", err)
	}

	rootKey := make([]byte, 33) // 32 zero padding + nibble-count = 0
	if val, err := txn.Get(dbi, rootKey); err == nil {
		t.Errorf("AccountsTrie has root-cache row at 33-zero-byte key (value %d bytes); reth's writer at provider.rs would never produce this row. value hex: %s",
			len(val), hex.EncodeToString(val))
	}

	cur, err := txn.OpenCursor(dbi)
	if err != nil {
		t.Fatalf("OpenCursor AccountsTrie: %v", err)
	}
	defer cur.Close()
	k, _, err := cur.Get(nil, nil, mdbx.First)
	if err != nil {
		// Empty table — vacuously satisfies the invariant.
		return
	}
	// In packed-key encoding the last byte is the nibble count. 0 = root.
	if len(k) == 33 && k[32] == 0 {
		t.Errorf("AccountsTrie cursor.First() returned a packed key with nibble count == 0 (root): %s", hex.EncodeToString(k))
	}
}

// checkStoragesTrieNoRoot asserts that no StoragesTrie entry has an
// all-zero packed SubKey (i.e., the first 33 bytes of the value are zero,
// meaning packed SubKey with nibble count = 0 = the storage-trie root).
func checkStoragesTrieNoRoot(t *testing.T, txn *mdbx.Txn) {
	t.Helper()
	dbi, err := txn.OpenDBI("StoragesTrie", 0, nil, nil)
	if err != nil {
		t.Fatalf("OpenDBI StoragesTrie: %v", err)
	}
	cur, err := txn.OpenCursor(dbi)
	if err != nil {
		t.Fatalf("OpenCursor StoragesTrie: %v", err)
	}
	defer cur.Close()

	zero33 := make([]byte, 33)
	var examined uint64
	var rootRows uint64
	k, v, err := cur.Get(nil, nil, mdbx.First)
	for ; err == nil; k, v, err = cur.Get(nil, nil, mdbx.Next) {
		examined++
		if len(v) >= 33 && bytes.Equal(v[:33], zero33) {
			rootRows++
			if rootRows <= 3 {
				t.Errorf("StoragesTrie has root-cache row (key=%s, packed SubKey all-zero); reth's writer at trie_cursor.rs would never produce this row",
					hex.EncodeToString(k))
			}
		}
	}
	if rootRows > 0 {
		t.Errorf("StoragesTrie has %d root-cache rows across %d examined entries (want 0)", rootRows, examined)
	}
	t.Logf("StoragesTrie: %d entries examined, %d root-cache rows", examined, rootRows)
}
