package generator

import (
	"log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
)

// mptTrieNodeWriter batches MPT trie node writes to a Pebble DB using
// geth's PathScheme key encoding ("A" + path for account nodes,
// "O" + accountHash + path for storage nodes).
//
// This mirrors the binary trie's trieNodeWriter in binary_stack_trie.go,
// but uses geth's rawdb helpers for MPT-specific key layout.
type mptTrieNodeWriter struct {
	db    ethdb.KeyValueStore
	batch ethdb.Batch
	nodes int
	bytes int64
}

func newMPTTrieNodeWriter(db ethdb.KeyValueStore) *mptTrieNodeWriter {
	return &mptTrieNodeWriter{
		db:    db,
		batch: db.NewBatch(),
	}
}

// accountCallback returns an OnTrieNode callback that persists account trie
// nodes with key = "A" + path (rawdb.TrieNodeAccountPrefix).
func (w *mptTrieNodeWriter) accountCallback() trie.OnTrieNode {
	return func(path []byte, hash common.Hash, blob []byte) {
		// Deep-copy: StackTrie docs warn path and blob are volatile.
		p := make([]byte, len(path))
		copy(p, path)
		b := make([]byte, len(blob))
		copy(b, blob)

		rawdb.WriteAccountTrieNode(w.batch, p, b)
		w.nodes++
		w.bytes += int64(1 + len(p) + len(b)) // "A" prefix + path + blob
		w.maybeFlush()
	}
}

// storageCallback returns an OnTrieNode callback that persists storage trie
// nodes with key = "O" + accountHash + path (rawdb.TrieNodeStoragePrefix).
func (w *mptTrieNodeWriter) storageCallback(accountHash common.Hash) trie.OnTrieNode {
	return func(path []byte, hash common.Hash, blob []byte) {
		p := make([]byte, len(path))
		copy(p, path)
		b := make([]byte, len(blob))
		copy(b, blob)

		rawdb.WriteStorageTrieNode(w.batch, accountHash, p, b)
		w.nodes++
		w.bytes += int64(1 + common.HashLength + len(p) + len(b)) // "O" + hash + path + blob
		w.maybeFlush()
	}
}

func (w *mptTrieNodeWriter) maybeFlush() {
	if w.batch.ValueSize() >= 256*1024*1024 {
		if err := w.batch.Write(); err != nil {
			log.Fatalf("failed to flush MPT trie node batch: %v", err)
		}
		w.batch.Reset()
	}
}

// flush writes any remaining buffered trie nodes to disk.
func (w *mptTrieNodeWriter) flush() {
	if w.batch.ValueSize() > 0 {
		if err := w.batch.Write(); err != nil {
			log.Fatalf("failed to flush MPT trie node batch: %v", err)
		}
		w.batch.Reset()
	}
}

// stats returns the number of trie nodes written and total bytes.
func (w *mptTrieNodeWriter) stats() (nodes int, bytes int64) {
	return w.nodes, w.bytes
}
