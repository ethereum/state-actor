//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/linxGnu/grocksdb"

	iReth "github.com/nerolation/state-actor/internal/reth"
)

// historyFlushThresholdBytes mirrors the besu writer's bulk-flush threshold:
// large enough to amortise RocksDB commit overhead, small enough to bound
// peak resident memory during archive-mode genesis writes.
const historyFlushThresholdBytes = 64 * 1024 * 1024

// historySink batches puts into reth v2's AccountsHistory and StoragesHistory
// column families (EitherReader::new_accounts_history /
// new_storages_history routes reads there under storage_v2).
//
// Durability is owed at Envs.Close (which FlushCFs the underlying
// memtables to SST files); per-flush we DisableWAL so the per-row append
// doesn't pay an fsync.
//
// NOT goroutine-safe; producers must serialise puts through one sink, or
// use one sink per goroutine and Flush each before merging results.
type historySink struct {
	db         *grocksdb.DB
	accountsCF *grocksdb.ColumnFamilyHandle
	storagesCF *grocksdb.ColumnFamilyHandle
	batch      *grocksdb.WriteBatch
	bytes      int
}

// newHistorySink builds a sink backed by envs.RocksDB + envs.RocksCFs.
// Caller must Close the sink (or call Flush before letting it go out of
// scope) to drain pending puts and release the underlying C++ WriteBatch.
func newHistorySink(envs *Envs) *historySink {
	return &historySink{
		db:         envs.RocksDB,
		accountsCF: envs.RocksCFs["AccountsHistory"],
		storagesCF: envs.RocksCFs["StoragesHistory"],
		batch:      grocksdb.NewWriteBatch(),
	}
}

// PutAccountHistory puts ShardedKeyAddress(addr, u64::MAX) →
// EncodeIntegerList([blockNum]) into the AccountsHistory column family.
func (s *historySink) PutAccountHistory(addr common.Address, blockNum uint64) error {
	shardedKey := iReth.ShardedKeyAddress{Address: addr, BlockNumber: ^uint64(0)}
	var keyBuf bytes.Buffer
	shardedKey.EncodeKey(&keyBuf)
	var listBuf bytes.Buffer
	iReth.EncodeIntegerList(&listBuf, []uint64{blockNum})
	s.batch.PutCF(s.accountsCF, keyBuf.Bytes(), listBuf.Bytes())
	s.bytes += keyBuf.Len() + listBuf.Len()
	return s.maybeFlush()
}

// PutStorageHistory puts StorageShardedKey(addr, slotKey, u64::MAX) →
// EncodeIntegerList([blockNum]) into the StoragesHistory column family.
func (s *historySink) PutStorageHistory(addr common.Address, slotKey common.Hash, blockNum uint64) error {
	ssk := iReth.StorageShardedKey{
		Address:     addr,
		StorageKey:  slotKey,
		BlockNumber: ^uint64(0),
	}
	var keyBuf bytes.Buffer
	ssk.EncodeKey(&keyBuf)
	var listBuf bytes.Buffer
	iReth.EncodeIntegerList(&listBuf, []uint64{blockNum})
	s.batch.PutCF(s.storagesCF, keyBuf.Bytes(), listBuf.Bytes())
	s.bytes += keyBuf.Len() + listBuf.Len()
	return s.maybeFlush()
}

// Flush drains the current batch via an async WAL-disabled write. Safe to
// call repeatedly; resets the batch on success.
func (s *historySink) Flush() error {
	if s.bytes == 0 {
		return nil
	}
	return s.flush()
}

// Close drains any pending writes and releases the underlying WriteBatch.
// Idempotent.
func (s *historySink) Close() error {
	if s.batch == nil {
		return nil
	}
	defer func() {
		s.batch.Destroy()
		s.batch = nil
	}()
	return s.Flush()
}

func (s *historySink) maybeFlush() error {
	if s.bytes < historyFlushThresholdBytes {
		return nil
	}
	return s.flush()
}

func (s *historySink) flush() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.DisableWAL(true)
	if err := s.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("reth history sink: flush: %w", err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}
