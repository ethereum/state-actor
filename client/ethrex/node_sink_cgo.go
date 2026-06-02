//go:build cgo_ethrex

package ethrex

import (
	"fmt"

	"github.com/linxGnu/grocksdb"
)

// flushThresholdBytes is the WriteBatch flush threshold during bulk import.
const flushThresholdBytes = 64 * 1024 * 1024

// batchSink wraps a grocksdb.WriteBatch targeting a specific CF and a shared
// flush mechanism. It is used for the trie-node CFs (account_trie_nodes and
// storage_trie_nodes) where NodeSink callbacks write (path, value) pairs.
type batchSink struct {
	db    *ethrexDB
	cfIdx int
	batch *grocksdb.WriteBatch
	bytes int
}

// newBatchSink constructs a batchSink for the given CF index.
// Caller must call Close() to flush any pending writes and free the WriteBatch.
func newBatchSink(db *ethrexDB, cfIdx int) *batchSink {
	return &batchSink{db: db, cfIdx: cfIdx, batch: grocksdb.NewWriteBatch()}
}

// put adds a key/value to the batch and triggers a flush if the threshold
// is exceeded.
func (s *batchSink) put(key, value []byte) error {
	s.batch.PutCF(s.db.cfs[s.cfIdx], key, value)
	s.bytes += len(key) + len(value)
	return s.maybeFlush()
}

// maybeFlush flushes the WriteBatch if the byte threshold is exceeded.
func (s *batchSink) maybeFlush() error {
	if s.bytes < flushThresholdBytes {
		return nil
	}
	return s.flushAsync()
}

func (s *batchSink) flushAsync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.DisableWAL(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("ethrex: flush batch (cf=%d): %w", s.cfIdx, err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}

func (s *batchSink) flushSync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.SetSync(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("ethrex: sync-flush batch (cf=%d): %w", s.cfIdx, err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}

// Close drains pending writes and releases the WriteBatch. Idempotent.
func (s *batchSink) Close() error {
	if s.batch == nil {
		return nil
	}
	defer func() {
		s.batch.Destroy()
		s.batch = nil
	}()
	if s.bytes == 0 {
		return nil
	}
	return s.flushSync()
}
