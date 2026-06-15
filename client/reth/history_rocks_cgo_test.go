//go:build cgo_reth

package reth

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/linxGnu/grocksdb"

	iReth "github.com/ethereum/state-actor/internal/reth"
)

// TestHistorySink_PutAndFlush exercises the basic write+read cycle: stage
// a few entries via PutAccountHistory + PutStorageHistory, Flush, then
// read the rows back via the RocksDB CF cursor and verify byte-identical
// key/value pairs.
func TestHistorySink_PutAndFlush(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	sink := newHistorySink(envs)
	defer sink.Close()

	addrA := common.Address{0x01, 0x02, 0x03}
	addrB := common.Address{0xAA, 0xBB}
	if err := sink.PutAccountHistory(addrA, 7); err != nil {
		t.Fatalf("PutAccountHistory: %v", err)
	}
	if err := sink.PutAccountHistory(addrB, 42); err != nil {
		t.Fatalf("PutAccountHistory: %v", err)
	}
	slotKey := common.Hash{0x10}
	if err := sink.PutStorageHistory(addrA, slotKey, 7); err != nil {
		t.Fatalf("PutStorageHistory: %v", err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()

	// AccountsHistory: ShardedKeyAddress(addr, u64::MAX) → EncodeIntegerList([blockNum])
	for _, tc := range []struct {
		addr     common.Address
		blockNum uint64
	}{{addrA, 7}, {addrB, 42}} {
		shardedKey := iReth.ShardedKeyAddress{Address: tc.addr, BlockNumber: ^uint64(0)}
		var keyBuf bytes.Buffer
		shardedKey.EncodeKey(&keyBuf)
		val, err := envs.RocksDB.GetCF(ro, envs.RocksCFs["AccountsHistory"], keyBuf.Bytes())
		if err != nil {
			t.Fatalf("GetCF AccountsHistory %s: %v", tc.addr.Hex(), err)
		}
		var listBuf bytes.Buffer
		iReth.EncodeIntegerList(&listBuf, []uint64{tc.blockNum})
		if !bytes.Equal(val.Data(), listBuf.Bytes()) {
			t.Errorf("AccountsHistory[%s] = %x, want %x", tc.addr.Hex(), val.Data(), listBuf.Bytes())
		}
		val.Free()
	}

	// StoragesHistory: StorageShardedKey(addr, slotKey, u64::MAX) → EncodeIntegerList([blockNum])
	ssk := iReth.StorageShardedKey{Address: addrA, StorageKey: slotKey, BlockNumber: ^uint64(0)}
	var sskBuf bytes.Buffer
	ssk.EncodeKey(&sskBuf)
	val, err := envs.RocksDB.GetCF(ro, envs.RocksCFs["StoragesHistory"], sskBuf.Bytes())
	if err != nil {
		t.Fatalf("GetCF StoragesHistory: %v", err)
	}
	defer val.Free()
	var listBuf bytes.Buffer
	iReth.EncodeIntegerList(&listBuf, []uint64{7})
	if !bytes.Equal(val.Data(), listBuf.Bytes()) {
		t.Errorf("StoragesHistory = %x, want %x", val.Data(), listBuf.Bytes())
	}
}

// TestHistorySink_CloseDrainsBatch checks the durability contract: writes
// staged via the sink but never explicitly flushed must still land on disk
// when Envs.Close runs (sink.Close drains the WriteBatch into the memtable
// + Envs.Close's FlushCFs forces the memtable to SST).
//
// Catches a regression where someone removes the FlushCFs in Envs.Close
// (silent data loss; reth would later see an empty AccountsHistory CF).
func TestHistorySink_CloseDrainsBatch(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}

	sink := newHistorySink(envs)
	addr := common.Address{0xCC, 0xDD}
	if err := sink.PutAccountHistory(addr, 99); err != nil {
		envs.Close()
		t.Fatalf("PutAccountHistory: %v", err)
	}
	// Deliberately skip sink.Flush(); rely on sink.Close + Envs.Close FlushCFs.
	if err := sink.Close(); err != nil {
		envs.Close()
		t.Fatalf("sink.Close: %v", err)
	}
	if err := envs.Close(); err != nil {
		t.Fatalf("Envs.Close: %v", err)
	}

	// Re-open and assert the row landed.
	envs2, err := OpenEnvs(tmp, false)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer envs2.Close()

	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	shardedKey := iReth.ShardedKeyAddress{Address: addr, BlockNumber: ^uint64(0)}
	var keyBuf bytes.Buffer
	shardedKey.EncodeKey(&keyBuf)
	val, err := envs2.RocksDB.GetCF(ro, envs2.RocksCFs["AccountsHistory"], keyBuf.Bytes())
	if err != nil {
		t.Fatalf("GetCF after reopen: %v", err)
	}
	defer val.Free()
	if val.Size() == 0 {
		t.Errorf("AccountsHistory[%s] not found after Envs.Close round-trip — FlushCFs may be missing", addr.Hex())
	}
}

// TestHistorySink_Threshold guards the historyFlushThresholdBytes
// auto-flush logic: enough puts to cumulatively exceed the threshold MUST
// trigger ≥1 in-band flush (s.bytes drops back below the threshold).
// Catches a regression that forgets to advance s.bytes or to compare
// against the constant. Skipped in -short.
func TestHistorySink_Threshold(t *testing.T) {
	if testing.Short() {
		t.Skip("Threshold test does bulk RocksDB writes; skipped in short mode")
	}
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	sink := newHistorySink(envs)
	defer sink.Close()

	// Each PutStorageHistory writes ~(53 key + small varint value) ≈ 63
	// bytes via PutCF. 2.0M puts ≈ 126 MiB → guaranteed to cross the
	// 64 MiB threshold at least once. Each crossing resets s.bytes to 0,
	// so the final s.bytes is bounded by one threshold's worth of puts.
	addr := common.Address{0xEF}
	const N = 2_000_000
	for i := uint64(0); i < N; i++ {
		var slot common.Hash
		slot[24] = byte(i >> 56)
		slot[25] = byte(i >> 48)
		slot[26] = byte(i >> 40)
		slot[27] = byte(i >> 32)
		slot[28] = byte(i >> 24)
		slot[29] = byte(i >> 16)
		slot[30] = byte(i >> 8)
		slot[31] = byte(i)
		if err := sink.PutStorageHistory(addr, slot, i); err != nil {
			t.Fatalf("PutStorageHistory[%d]: %v", i, err)
		}
	}
	if sink.bytes >= historyFlushThresholdBytes {
		t.Errorf("after %d puts, s.bytes=%d >= threshold %d — auto-flush never triggered",
			N, sink.bytes, historyFlushThresholdBytes)
	}
}
