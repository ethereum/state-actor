//go:build cgo_reth

package reth

import (
	"bytes"
	"testing"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// countMDBXRows returns the row count of an MDBX table by full scan. Used
// by both the FullMode and Archive tests below.
func countMDBXRows(t *testing.T, envs *Envs, table string) int {
	t.Helper()
	var count int
	_ = envs.Mdbx.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(envs.MdbxDBIs[table])
		if err != nil {
			return err
		}
		defer cur.Close()
		for _, _, err := cur.Get(nil, nil, mdbx.First); err == nil; _, _, err = cur.Get(nil, nil, mdbx.Next) {
			count++
		}
		return nil
	})
	return count
}

func TestWriteContractStorageRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	addr := common.HexToAddress("0xdeadbeef")
	addrHash := crypto.Keccak256Hash(addr[:])
	contract := &entitygen.Account{
		Address:  addr,
		AddrHash: addrHash,
		StateAccount: &types.StateAccount{
			Nonce:   1,
			Balance: uint256.NewInt(0),
		},
		Storage: []entitygen.StorageSlot{
			{Key: common.HexToHash("0x01"), Value: common.HexToHash("0xa")},
			{Key: common.HexToHash("0x02"), Value: common.HexToHash("0xb")},
			{Key: common.HexToHash("0x03"), Value: common.HexToHash("0xc")},
		},
	}

	err = envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		_, err := WriteContractStorage(envs, txn, contract, 0, true /* archive */)
		return err
	})
	if err != nil {
		t.Fatalf("WriteContractStorage: %v", err)
	}

	// v2 invariant: PlainStorageState MUST be empty.
	if got := countMDBXRows(t, envs, "PlainStorageState"); got != 0 {
		t.Errorf("PlainStorageState rows = %d, want 0 (empty on v2)", got)
	}

	// HashedStorages: count under addrHash via DupSort cursor.
	if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(envs.MdbxDBIs["HashedStorages"])
		if err != nil {
			return err
		}
		defer cur.Close()
		count := 0
		for k, _, err := cur.Get(addrHash[:], nil, mdbx.SetKey); err == nil; k, _, err = cur.Get(nil, nil, mdbx.NextDup) {
			if !bytes.Equal(k, addrHash[:]) {
				break
			}
			count++
		}
		if count != len(contract.Storage) {
			t.Errorf("HashedStorages: %d entries, want %d", count, len(contract.Storage))
		}
		return nil
	}); err != nil {
		t.Errorf("verify HashedStorages: %v", err)
	}

	// Spot-check StoragesHistory in the RocksDB CF (v2 routing) for slot
	// 0x01. Flush the sink first so the WAL-disabled batch lands in the
	// memtable where GetCF can see it.
	if err := envs.HistorySink().Flush(); err != nil {
		t.Fatalf("historySink.Flush: %v", err)
	}
	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	ssk := iReth.StorageShardedKey{
		Address:     addr,
		StorageKey:  common.HexToHash("0x01"),
		BlockNumber: ^uint64(0),
	}
	var keyBuf bytes.Buffer
	ssk.EncodeKey(&keyBuf)
	val, err := envs.RocksDB.GetCF(ro, envs.RocksCFs["StoragesHistory"], keyBuf.Bytes())
	if err != nil {
		t.Fatalf("GetCF StoragesHistory: %v", err)
	}
	defer val.Free()
	list, _ := iReth.DecodeIntegerList(val.Data())
	if len(list) != 1 || list[0] != 0 {
		t.Errorf("StoragesHistory list = %v, want [0]", list)
	}

	// MDBX StoragesHistory MUST be empty under v2 (data lives in RocksDB).
	if got := countMDBXRows(t, envs, "StoragesHistory"); got != 0 {
		t.Errorf("MDBX StoragesHistory rows = %d, want 0 (v2 routes to RocksDB CF)", got)
	}
}

// TestWriteContractStorage_FullMode: with archive=false (full mode, the
// default), WriteContractStorage populates HashedStorages and elides both
// archive-only tables. Under v2, PlainStorageState is empty regardless of
// archive mode.
func TestWriteContractStorage_FullMode(t *testing.T) {
	envs, err := OpenEnvs(t.TempDir(), true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	addr := common.HexToAddress("0xcafef00d")
	contract := &entitygen.Account{
		Address:  addr,
		AddrHash: crypto.Keccak256Hash(addr[:]),
		StateAccount: &types.StateAccount{
			Nonce:   1,
			Balance: uint256.NewInt(0),
		},
		Storage: []entitygen.StorageSlot{
			{Key: common.HexToHash("0x01"), Value: common.HexToHash("0xa")},
			{Key: common.HexToHash("0x02"), Value: common.HexToHash("0xb")},
		},
	}

	err = envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		_, err := WriteContractStorage(envs, txn, contract, 0, false /* archive */)
		return err
	})
	if err != nil {
		t.Fatalf("WriteContractStorage(archive=false): %v", err)
	}

	if got := countMDBXRows(t, envs, "StorageChangeSets"); got != 0 {
		t.Errorf("StorageChangeSets rows = %d, want 0 (full mode skips)", got)
	}
	if got := countMDBXRows(t, envs, "StoragesHistory"); got != 0 {
		t.Errorf("MDBX StoragesHistory rows = %d, want 0 (full mode skips + v2 routes to RocksDB)", got)
	}
	if got := countMDBXRows(t, envs, "PlainStorageState"); got != 0 {
		t.Errorf("PlainStorageState rows = %d, want 0 (empty on v2)", got)
	}
	if got := countMDBXRows(t, envs, "HashedStorages"); got != len(contract.Storage) {
		t.Errorf("HashedStorages rows = %d, want %d", got, len(contract.Storage))
	}
}

// TestWriteContractStorage_Archive: with archive=true, HashedStorages,
// StorageChangeSets, and the RocksDB StoragesHistory CF populate at the
// expected counts. PlainStorageState stays empty (v2 invariant) and MDBX
// StoragesHistory stays empty (v2 routes to RocksDB).
func TestWriteContractStorage_Archive(t *testing.T) {
	envs, err := OpenEnvs(t.TempDir(), true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	addr := common.HexToAddress("0xcafef00d")
	contract := &entitygen.Account{
		Address:  addr,
		AddrHash: crypto.Keccak256Hash(addr[:]),
		StateAccount: &types.StateAccount{
			Nonce:   1,
			Balance: uint256.NewInt(0),
		},
		Storage: []entitygen.StorageSlot{
			{Key: common.HexToHash("0x01"), Value: common.HexToHash("0xa")},
			{Key: common.HexToHash("0x02"), Value: common.HexToHash("0xb")},
		},
	}

	err = envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		_, err := WriteContractStorage(envs, txn, contract, 0, true /* archive */)
		return err
	})
	if err != nil {
		t.Fatalf("WriteContractStorage(archive=true): %v", err)
	}

	// Drain the RocksDB sink so the StoragesHistory CF read returns rows.
	if err := envs.HistorySink().Flush(); err != nil {
		t.Fatalf("historySink.Flush: %v", err)
	}

	if got := countMDBXRows(t, envs, "PlainStorageState"); got != 0 {
		t.Errorf("PlainStorageState rows = %d, want 0 (empty on v2)", got)
	}
	if got := countMDBXRows(t, envs, "HashedStorages"); got != len(contract.Storage) {
		t.Errorf("HashedStorages rows = %d, want %d", got, len(contract.Storage))
	}
	if got := countMDBXRows(t, envs, "StorageChangeSets"); got != len(contract.Storage) {
		t.Errorf("StorageChangeSets rows = %d, want %d", got, len(contract.Storage))
	}
	if got := countMDBXRows(t, envs, "StoragesHistory"); got != 0 {
		t.Errorf("MDBX StoragesHistory rows = %d, want 0 (v2 routes to RocksDB CF)", got)
	}

	// RocksDB CF: should have one row per storage slot.
	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	iter := envs.RocksDB.NewIteratorCF(ro, envs.RocksCFs["StoragesHistory"])
	defer iter.Close()
	cfCount := 0
	for iter.SeekToFirst(); iter.Valid(); iter.Next() {
		cfCount++
	}
	if cfCount != len(contract.Storage) {
		t.Errorf("RocksDB StoragesHistory CF rows = %d, want %d", cfCount, len(contract.Storage))
	}
}
