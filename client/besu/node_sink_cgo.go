//go:build cgo_besu

package besu

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/besu/keys"
)

// flushThresholdBytes is the WriteBatch flush threshold during bulk import,
// sized to amortise RocksDB commit overhead.
const flushThresholdBytes = 64 * 1024 * 1024

// nodeSink implements internal/besu/trie.NodeSink and is also the central
// sink for flat-state writes from state_writer_cgo.go's Phase 2 loop.
//
// Trie-node writes and flat-state writes share a SINGLE WriteBatch. Reasons:
//   - Cross-CF atomicity: in a single RocksDB instance, Put + flush of one
//     batch is the only atomic-across-CFs primitive. Two parallel batches
//     can interleave their flushes such that a crash leaves
//     ACCOUNT_INFO_STATE writes durable but TRIE_BRANCH_STORAGE writes
//     not (or vice versa) — causing genesis-block stateRoot mismatch on
//     next boot.
//   - Memory bound: one batch capped at 16 MiB is simpler to reason about
//     than two.
//
// Phase 2 streaming flow:
//   1. Per-account: NodeSink methods PutAccountStorageTrieNode and
//      PutAccountStateTrieNode add trie-node writes. PutFlatAccount,
//      PutFlatStorage, PutCode add flat-state writes (helper methods on
//      *nodeSink, not on the NodeSink interface).
//   2. After every put, maybeFlush checks the running byte count. If
//      ≥16 MiB, the batch is flushed (with sync=false; default fsync
//      cadence) and reset.
//   3. After all entities, SaveWorldState writes the three world-state
//      sentinels and triggers a final flush (with sync=true).
//   4. The genesis block writes (genesis_cgo.go) bypass nodeSink and use
//      direct putSync calls to guarantee chainHeadHash lands LAST and
//      durably.
type nodeSink struct {
	db    *besuDB
	batch *grocksdb.WriteBatch
	bytes int
}

// newNodeSink constructs a sink backed by the given DB. Caller must call
// Close (or have SaveWorldState called once) before letting the sink go
// out of scope; otherwise pending writes are silently dropped AND the
// underlying WriteBatch leaks C++ memory.
func newNodeSink(db *besuDB) *nodeSink {
	return &nodeSink{db: db, batch: grocksdb.NewWriteBatch()}
}

// PutAccountStateTrieNode writes a Bonsai account-trie node at TRIE_BRANCH_STORAGE[location].
// Empty-trie short-circuit per BonsaiWorldStateKeyValueStorage.java:286-288
// is enforced upstream by the trie builder.
func (s *nodeSink) PutAccountStateTrieNode(location []byte, hash common.Hash, nodeRLP []byte) error {
	s.batch.PutCF(s.db.cfs[cfIdxTrieBranchStorage], location, nodeRLP)
	s.bytes += len(location) + len(nodeRLP)
	return s.maybeFlush()
}

// PutAccountStorageTrieNode writes a per-account storage-trie node at
// TRIE_BRANCH_STORAGE[accountHash(32) ++ location] per
// BonsaiWorldStateKeyValueStorage.java:306-309.
func (s *nodeSink) PutAccountStorageTrieNode(addrHash common.Hash, location []byte, hash common.Hash, nodeRLP []byte) error {
	fullKey := make([]byte, 32+len(location))
	copy(fullKey, addrHash[:])
	copy(fullKey[32:], location)
	s.batch.PutCF(s.db.cfs[cfIdxTrieBranchStorage], fullKey, nodeRLP)
	s.bytes += len(fullKey) + len(nodeRLP)
	return s.maybeFlush()
}

// SaveWorldState writes the three world-state sentinels per
// BonsaiWorldStateKeyValueStorage.java:273-282:
//
//   - TRIE_BRANCH_STORAGE[Bytes.EMPTY]      = rootRLP
//   - TRIE_BRANCH_STORAGE["worldRoot"]      = rootHash
//   - TRIE_BRANCH_STORAGE["worldBlockHash"] = blockHash
//
// All three writes go in the same batch as any pending trie/flat-state
// writes, then a final flush commits everything atomically. The flush is
// synchronous (sync=true) to guarantee the world-state is durable before
// the caller proceeds to write block keys + chainHeadHash.
func (s *nodeSink) SaveWorldState(blockHash, rootHash common.Hash, rootRLP []byte) error {
	s.batch.PutCF(s.db.cfs[cfIdxTrieBranchStorage], []byte{}, rootRLP)
	s.batch.PutCF(s.db.cfs[cfIdxTrieBranchStorage], keys.WorldRootKey, rootHash[:])
	s.batch.PutCF(s.db.cfs[cfIdxTrieBranchStorage], keys.WorldBlockHashKey, blockHash[:])
	s.bytes += len(rootRLP) + 32 + 32
	return s.flushSync()
}

// PutFlatAccount writes ACCOUNT_INFO_STATE[keccak256(addr)] = accountRLP.
// Per BonsaiAccount.writeTo / PathBasedAccount.serializeAccount.
//
// Skip the write if accountRLP is empty (matches
// BonsaiWorldStateKeyValueStorage.java:263-271 putAccountInfoState behavior).
func (s *nodeSink) PutFlatAccount(addrHash common.Hash, accountRLP []byte) error {
	if len(accountRLP) == 0 {
		return nil
	}
	s.batch.PutCF(s.db.cfs[cfIdxAccountInfoState], addrHash[:], accountRLP)
	s.bytes += 32 + len(accountRLP)
	return s.maybeFlush()
}

// PutFlatStorage writes ACCOUNT_STORAGE_STORAGE[addrHash ++ slotHash] = valueRLP.
// Per PathBasedWorldView.encodeTrieValue (zero slots are valueRLP=[0x80]).
func (s *nodeSink) PutFlatStorage(addrHash, slotHash common.Hash, valueRLP []byte) error {
	key := make([]byte, 64)
	copy(key, addrHash[:])
	copy(key[32:], slotHash[:])
	s.batch.PutCF(s.db.cfs[cfIdxAccountStorageStorage], key, valueRLP)
	s.bytes += 64 + len(valueRLP)
	return s.maybeFlush()
}

// PutCode writes CODE_STORAGE[codeHash] = code (code-hash-keyed default).
// Skip empty code per BonsaiWorldStateKeyValueStorage.java:248-255.
func (s *nodeSink) PutCode(codeHash common.Hash, code []byte) error {
	if len(code) == 0 {
		return nil
	}
	s.batch.PutCF(s.db.cfs[cfIdxCodeStorage], codeHash[:], code)
	s.bytes += 32 + len(code)
	return s.maybeFlush()
}

// maybeFlush flushes the batch if its accumulated byte count exceeds the
// threshold. Async flush (sync=false) — fsync ordering at end-of-run is
// guaranteed by the SaveWorldState (sync=true) and the chainHeadHash
// putSync in genesis_cgo.go.
func (s *nodeSink) maybeFlush() error {
	if s.bytes < flushThresholdBytes {
		return nil
	}
	return s.flushAsync()
}

func (s *nodeSink) flushAsync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	// Disable WAL on bulk flushes. Genesis is one-shot; durability is
	// owed by flushSync/putSync (SaveWorldState + chainHeadHash) which
	// fsync independently.
	wo.DisableWAL(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("besu: flush batch: %w", err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}

func (s *nodeSink) flushSync() error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.SetSync(true)
	if err := s.db.db.Write(wo, s.batch); err != nil {
		return fmt.Errorf("besu: sync-flush batch: %w", err)
	}
	s.batch.Clear()
	s.bytes = 0
	return nil
}

// Close drains any pending writes and releases the WriteBatch. Idempotent.
func (s *nodeSink) Close() error {
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
