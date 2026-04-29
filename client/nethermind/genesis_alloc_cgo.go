//go:build cgo_neth

package nethermind

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/linxGnu/grocksdb"

	"github.com/nerolation/state-actor/internal/neth"
	nethrlp "github.com/nerolation/state-actor/internal/neth/rlp"
	nethstorage "github.com/nerolation/state-actor/internal/neth/storage"
	nethtrie "github.com/nerolation/state-actor/internal/neth/trie"
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

// stateBatchFlushBytes is the WriteBatch flush threshold. 16 MiB hits a
// good balance between per-flush overhead (small batches → many fsyncs)
// and peak memory (large batches → big in-memory queue). Tune by
// benchmarking write throughput vs. memory headroom on the host.
const stateBatchFlushBytes = 16 * 1024 * 1024

func newStateDBSink(db *grocksdb.DB) *stateDBSink {
	return &stateDBSink{
		db: db,
		wo: grocksdb.NewDefaultWriteOptions(),
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

// writeGenesisAllocAccounts walks the genesis allocation, writes each
// account's leaf into the state trie via trie.Builder, and returns the
// computed state root. Code bytes (if any) go into the code DB keyed by
// keccak(code). Storage slots (if any) go through the per-account storage
// trie and the resulting root replaces account.Root before encoding.
//
// Accounts MUST be processed in keccak(address) ascending order — that's
// what the StackTrie inside Builder requires. We sort them up-front, and
// per-account slot keys get sorted by keccak(slotKey) the same way.
//
// `storages` may be nil or empty when the alloc carries no contract
// storage; the per-account storage-trie path is skipped and only the
// account-trie is built.
func writeGenesisAllocAccounts(
	dbs *nethDBs,
	accounts map[common.Address]*types.StateAccount,
	codes map[common.Address][]byte,
	storages map[common.Address]map[common.Hash]common.Hash,
) (common.Hash, error) {
	if len(accounts) == 0 {
		return common.Hash(neth.EmptyTreeHash), nil
	}

	sink := newStateDBSink(dbs.state)
	defer func() { _ = sink.close() }()

	builder := nethtrie.NewBuilder(sink)

	type addrEntry struct {
		addrHash [32]byte
		addr     common.Address
	}

	entries := make([]addrEntry, 0, len(accounts))
	for addr := range accounts {
		var ah [32]byte
		copy(ah[:], crypto.Keccak256(addr.Bytes()))
		entries = append(entries, addrEntry{addrHash: ah, addr: addr})
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].addrHash[:], entries[j].addrHash[:]) < 0
	})

	codeWO := grocksdb.NewDefaultWriteOptions()
	defer codeWO.Destroy()

	for _, e := range entries {
		acc := accounts[e.addr]

		// Write code DB if the account has bytecode. Nethermind reads code
		// by keccak(code), so the key here mirrors what the EVM looks up.
		if code := codes[e.addr]; len(code) > 0 {
			codeHash := crypto.Keccak256Hash(code)
			if err := dbs.code.Put(codeWO, codeHash[:], code); err != nil {
				return common.Hash{}, fmt.Errorf("write code for %s: %w", e.addr.Hex(), err)
			}
			acc.CodeHash = codeHash[:]
		}

		// Build the account's storage trie if it has any non-zero slots.
		// Slots must enter the StackTrie in keccak(slotKey)-ascending
		// order (same discipline as the account-trie above), so we sort
		// the (keyHash, value) pairs before feeding the Builder.
		if slots := storages[e.addr]; len(slots) > 0 {
			type hashedSlot struct {
				keyHash common.Hash
				value   common.Hash
			}
			hashed := make([]hashedSlot, 0, len(slots))
			for k, v := range slots {
				hashed = append(hashed, hashedSlot{
					keyHash: crypto.Keccak256Hash(k[:]),
					value:   v,
				})
			}
			sort.Slice(hashed, func(i, j int) bool {
				return bytes.Compare(hashed[i].keyHash[:], hashed[j].keyHash[:]) < 0
			})

			for _, s := range hashed {
				v := s.value[:]
				for len(v) > 0 && v[0] == 0 {
					v = v[1:]
				}
				if len(v) == 0 {
					// Zero slots are deletions; skipping matches geth/Nethermind.
					continue
				}
				valRLP, err := gethrlp.EncodeToBytes(v)
				if err != nil {
					return common.Hash{}, fmt.Errorf("encode slot value for %s: %w", e.addr.Hex(), err)
				}
				if err := builder.AddStorageSlot(e.addrHash, [32]byte(s.keyHash), valRLP); err != nil {
					return common.Hash{}, fmt.Errorf("add storage slot for %s: %w", e.addr.Hex(), err)
				}
			}
			storageRoot, err := builder.FinalizeStorageRoot(e.addrHash)
			if err != nil {
				return common.Hash{}, fmt.Errorf("finalize storage root for %s: %w", e.addr.Hex(), err)
			}
			acc.Root = common.Hash(storageRoot)
		}

		accRLP, err := nethrlp.EncodeAccount(acc)
		if err != nil {
			return common.Hash{}, fmt.Errorf("encode account %s: %w", e.addr.Hex(), err)
		}
		if err := builder.AddAccount(e.addrHash, accRLP); err != nil {
			return common.Hash{}, fmt.Errorf("add account %s: %w", e.addr.Hex(), err)
		}
	}

	root, err := builder.FinalizeStateRoot()
	if err != nil {
		return common.Hash{}, fmt.Errorf("finalize state root: %w", err)
	}
	// Flush before returning so callers that read the State DB next
	// (e.g., reopening it for inspection) see all the trie nodes.
	if err := sink.close(); err != nil {
		return common.Hash{}, fmt.Errorf("flush state writes: %w", err)
	}
	return common.Hash(root), nil
}
