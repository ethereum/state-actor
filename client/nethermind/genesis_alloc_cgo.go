//go:build cgo_neth

package nethermind

import (
	"fmt"
	"sync"

	"github.com/linxGnu/grocksdb"
)

// stateBatchFlushBytes is the WriteBatch flush threshold, sized to amortise
// RocksDB commit overhead. Shared by codeDBSink and the flat sink.
const stateBatchFlushBytes = 64 * 1024 * 1024

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
	// Skip the WAL on bulk flushes; durability is owed at the final
	// CompactRange + Close (same rationale as the flat sink).
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
