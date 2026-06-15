//go:build cgo_neth

package nethermind

import (
	"fmt"
	"sync"

	"github.com/linxGnu/grocksdb"

	nethstorage "github.com/ethereum/state-actor/internal/neth/storage"
)

// stateDBSink writes state-trie nodes to the State DB using HalfPath keys.
// This is the bridge between internal/neth/trie.Builder (which emits
// OnTrieNode callbacks) and the State RocksDB Nethermind reads on boot.
//
// Writes are buffered into a grocksdb WriteBatch and flushed when the
// pending size hits stateBatchFlushBytes — synchronous Put-per-node went
// fsync-bound at 5M+500K scale. The batch is flushed (and the sink can
// be safely closed) by calling flush() before reading the State DB.
type stateDBSink struct {
	db *grocksdb.DB
	wo *grocksdb.WriteOptions
	wb *grocksdb.WriteBatch

	// pendingBytes tracks the live WriteBatch's payload size; we flush
	// when it crosses stateBatchFlushBytes to keep memory bounded for
	// 50GB-scale runs that emit hundreds of millions of trie nodes.
	pendingBytes int
}

// stateBatchFlushBytes is the WriteBatch flush threshold, sized to amortise
// RocksDB commit overhead.
const stateBatchFlushBytes = 64 * 1024 * 1024

func newStateDBSink(db *grocksdb.DB) *stateDBSink {
	wo := grocksdb.NewDefaultWriteOptions()
	// Disable WAL on bulk flushes. Durability is owed at the final
	// CompactRange + Close.
	wo.DisableWAL(true)
	return &stateDBSink{
		db: db,
		wo: wo,
		wb: grocksdb.NewWriteBatch(),
	}
}

// flush writes any pending entries and resets the WriteBatch. Safe to
// call repeatedly — a no-op when nothing is buffered.
func (s *stateDBSink) flush() error {
	if s.pendingBytes == 0 {
		return nil
	}
	if err := s.db.Write(s.wo, s.wb); err != nil {
		return fmt.Errorf("stateDBSink flush: %w", err)
	}
	s.wb.Clear()
	s.pendingBytes = 0
	return nil
}

func (s *stateDBSink) close() error {
	err := s.flush()
	if s.wb != nil {
		s.wb.Destroy()
		s.wb = nil
	}
	if s.wo != nil {
		s.wo.Destroy()
		s.wo = nil
	}
	return err
}

func (s *stateDBSink) put(key, value []byte) error {
	s.wb.Put(key, value)
	s.pendingBytes += len(key) + len(value)
	if s.pendingBytes >= stateBatchFlushBytes {
		return s.flush()
	}
	return nil
}

func (s *stateDBSink) SetStateNode(path []byte, pathLen int, keccak [32]byte, rlpBlob []byte) error {
	return s.put(nethstorage.StateNodeKey(path, pathLen, keccak), rlpBlob)
}

// SetStorageNode writes a storage-trie node at its HalfPath storage key
// (74 bytes: section(=2) + addrHash(32) + path[:8] + pathLen + keccak).
func (s *stateDBSink) SetStorageNode(addrHash [32]byte, path []byte, pathLen int, keccak [32]byte, rlpBlob []byte) error {
	return s.put(nethstorage.StorageNodeKey(addrHash, path, pathLen, keccak), rlpBlob)
}

// codeDBSink batches writes to the dbs.code RocksDB at stateBatchFlushBytes
// and is safe for concurrent callers via the internal mutex (Phase 0 workers
// share one instance).
type codeDBSink struct {
	db           *grocksdb.DB
	wo           *grocksdb.WriteOptions
	wb           *grocksdb.WriteBatch
	mu           sync.Mutex
	pendingBytes int
}

func newCodeDBSink(db *grocksdb.DB) *codeDBSink {
	wo := grocksdb.NewDefaultWriteOptions()
	// Same WAL-skip rationale as stateDBSink (line 38-43).
	wo.DisableWAL(true)
	return &codeDBSink{
		db: db,
		wo: wo,
		wb: grocksdb.NewWriteBatch(),
	}
}

func (s *codeDBSink) put(key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wb.Put(key, value)
	s.pendingBytes += len(key) + len(value)
	if s.pendingBytes >= stateBatchFlushBytes {
		return s.flushLocked()
	}
	return nil
}

// flushLocked writes any pending entries and resets the WriteBatch.
// Caller must hold s.mu. Safe to call repeatedly — a no-op when nothing
// is buffered.
func (s *codeDBSink) flushLocked() error {
	if s.pendingBytes == 0 {
		return nil
	}
	if err := s.db.Write(s.wo, s.wb); err != nil {
		return fmt.Errorf("codeDBSink flush: %w", err)
	}
	s.wb.Clear()
	s.pendingBytes = 0
	return nil
}

func (s *codeDBSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.flushLocked()
	if s.wb != nil {
		s.wb.Destroy()
		s.wb = nil
	}
	if s.wo != nil {
		s.wo.Destroy()
		s.wo = nil
	}
	return err
}
