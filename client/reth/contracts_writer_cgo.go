//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// storagesTrieTxnRows caps one drainStoragesTrie transaction: a mainnet-size
// StoragesTrie (tens of GB) in a single txn degrades MDBX's dirty-page
// tracking the same way streamSpecStorage's per-entity txns avoid.
const storagesTrieTxnRows = 1_000_000

// storagesTrieEmitter spills addrHash's stored storage-trie branches into
// trieRows for drainStoragesTrie. The whole row (addrHash || 33-byte packed
// SubKey || BNC) is the sort key with an empty value, so the store yields
// rows in exactly MDBX's (main key, dup value) order, and an identical row
// collapses as it would in the DupSort table.
func storagesTrieEmitter(trieRows *streamsort.Store, addrHash common.Hash) iReth.NodeEmitter {
	return func(path iReth.StoredNibbles, node iReth.BranchNodeCompact) error {
		if path.Length == 0 {
			// Skip root branch — see TestNoRootCacheRows.
			return nil
		}
		var buf bytes.Buffer
		buf.Write(addrHash[:])
		entry := iReth.StorageTrieEntry{SubKey: path, Node: node}
		entry.EncodePackedCompact(&buf)
		return trieRows.Put(buf.Bytes(), nil)
	}
}

// drainStoragesTrie appends every spilled row to StoragesTrie. Writers run
// in contract generation order, so a direct Put lands each contract's main
// key at a random point in the table's top-level tree and leaves its pages
// about half full; appending the globally sorted rows fills them densely.
// Must run after the last StoragesTrie writer: Append rejects a key below
// the table's current last key.
func drainStoragesTrie(envs *Envs, trieRows *streamsort.Store) error {
	it, err := trieRows.NewCursor()
	if err != nil {
		return err
	}
	defer it.Close()
	for it.Valid() {
		err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
			if err := envs.useFill("StoragesTrie"); err != nil {
				return fmt.Errorf("useFill(StoragesTrie): %w", err)
			}
			cur, err := txn.OpenCursor(envs.MdbxDBIs["StoragesTrie"])
			if err != nil {
				return fmt.Errorf("open StoragesTrie cursor: %w", err)
			}
			defer cur.Close()
			for n := 0; n < storagesTrieTxnRows && it.Valid(); n++ {
				row := it.Key()
				if err := cur.Put(row[:common.HashLength], row[common.HashLength:], mdbx.Append|mdbx.AppendDup); err != nil {
					return fmt.Errorf("StoragesTrie append %x: %w", row[:common.HashLength], err)
				}
				it.Next()
			}
			return it.Err()
		})
		if err != nil {
			return err
		}
	}
	return it.Err()
}

// WriteContracts writes the v2 contract tables: Bytecodes (deduped),
// AccountChangeSets (archive), HashedStorages, StorageChangeSets (archive).
// AccountsHistory + StoragesHistory rows (archive) are routed to the
// RocksDB CFs via envs.HistorySink(). StoragesTrie rows are spilled into
// trieRows for drainStoragesTrie.
//
// HashedAccounts is written by RunCgo's phase 2, not here (see
// hashedAccountValue in run_cgo.go).
//
// SIDE EFFECT: each contract's StateAccount.Root and .CodeHash are
// mutated in place from the supplied Storage + Code. With empty Storage
// the existing Root is preserved (the spec-storage streaming phase sets
// it ahead of time); a zero Root in that case is rejected.
//
// stats (optional) accumulates CodeBytes (deduped — only counts code
// that actually got written) and StorageBytes (sum of HashedStorages
// compact-encoded entries). Increments are applied only after the MDBX
// transaction commits.
func WriteContracts(envs *Envs, trieRows *streamsort.Store, contracts []*entitygen.Account, blockNum uint64, archive bool, stats *generator.Stats) error {
	var (
		localStorageBytes uint64
		localCodeBytes    uint64
	)
	err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		blockKey := beU64(blockNum)

		// Shared BytecodeWriter deduplicates across all contracts in this call.
		bw := NewBytecodeWriter(txn, envs.MdbxDBIs["Bytecodes"], 100_000)

		for _, contract := range contracts {
			if contract.StateAccount == nil {
				return fmt.Errorf("WriteContracts: contract %s has nil StateAccount", contract.Address.Hex())
			}

			var storageRoot common.Hash
			if len(contract.Storage) > 0 {
				// Per-contract StoragesTrie emit: the intermediate-node cache
				// reth's payload builder reads during state-root computation.
				var err error
				storageRoot, err = computeStorageRoot(contract.Storage, storagesTrieEmitter(trieRows, contract.AddrHash))
				if err != nil {
					return fmt.Errorf("WriteContracts: computeStorageRoot %s: %w", contract.Address.Hex(), err)
				}
			} else {
				storageRoot = contract.StateAccount.Root
				if storageRoot == (common.Hash{}) {
					return fmt.Errorf("WriteContracts: contract %s has empty Storage AND zero StateAccount.Root — "+
						"caller must set Root (e.g. types.EmptyRootHash) before calling WriteContracts",
						contract.Address.Hex())
				}
			}

			if err := envs.useFill("Bytecodes"); err != nil {
				return fmt.Errorf("WriteContracts: useFill(Bytecodes): %w", err)
			}
			codeHash, wrote, err := bw.Write(contract.Code)
			if err != nil {
				return fmt.Errorf("WriteContracts: bytecode write %s: %w", contract.Address.Hex(), err)
			}
			if wrote {
				localCodeBytes += uint64(len(contract.Code))
			}

			contract.StateAccount.Root = storageRoot
			contract.StateAccount.CodeHash = codeHash.Bytes()

			if archive {
				// AccountChangeSets: DupSort BE_u64(block) → AccountBeforeTx{addr, nil}
				if err := envs.useFill("AccountChangeSets"); err != nil {
					return fmt.Errorf("WriteContracts: useFill(AccountChangeSets): %w", err)
				}
				abt := iReth.AccountBeforeTx{Address: contract.Address, Info: nil}
				var abtBuf bytes.Buffer
				abt.EncodeCompact(&abtBuf)
				if err := txn.Put(envs.MdbxDBIs["AccountChangeSets"], blockKey[:], abtBuf.Bytes(), 0); err != nil {
					return fmt.Errorf("AccountChangeSets %s: %w", contract.Address.Hex(), err)
				}
				// AccountsHistory → RocksDB CF (v2 routing).
				if err := envs.HistorySink().PutAccountHistory(contract.Address, blockNum); err != nil {
					return fmt.Errorf("AccountsHistory %s: %w", contract.Address.Hex(), err)
				}
			}

			if err := envs.useFill("HashedStorages"); err != nil {
				return fmt.Errorf("WriteContracts: useFill(HashedStorages): %w", err)
			}
			storBytes, err := WriteContractStorage(envs, txn, contract, blockNum, archive)
			if err != nil {
				return fmt.Errorf("WriteContracts: WriteContractStorage %s: %w", contract.Address.Hex(), err)
			}

			localStorageBytes += storBytes
		}
		return nil
	})
	if err != nil {
		return err
	}
	if stats != nil {
		stats.StorageBytes += localStorageBytes
		stats.CodeBytes += localCodeBytes
	}
	return nil
}
