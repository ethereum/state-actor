//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// WriteContractStorage writes per-slot rows for a single contract across
// the v2 storage tables. accounts must be a contract account (StateAccount
// + Storage slots populated). blockNum is the block at which storage was
// newly written (0 for genesis). All slots are written within ONE MDBX
// transaction (the caller's).
//
// Per-slot writes:
//   - HashedStorages (DupSort): keccak(Address) → StorageEntry{keccak(slot_key), slot_value} — canonical v2 state, always.
//   - StorageChangeSets (DupSort): BlockNumberAddress → StorageEntry{slot_key, prev=0} — archive only.
//   - StoragesHistory: StorageShardedKey{addr, slot_key, u64::MAX} → IntegerList([block]) — archive only; routed to the RocksDB CF via envs.HistorySink().
//
// For genesis (blockNum=0) the "before" value in StorageChangeSets is 0 —
// the slot was newly set.
//
// Returns the sum of HashedStorages entry sizes (compact encoding) across
// all slots, for the caller's stats accumulator. Auxiliary tables and
// their keys are not counted. Caller transfers the count to
// *generator.Stats only after the enclosing MDBX txn commits.
func WriteContractStorage(
	envs *Envs,
	txn *mdbx.Txn,
	contract *entitygen.Account,
	blockNum uint64,
	archive bool,
) (uint64, error) {
	blockKey := iReth.BlockNumberAddress{BlockNumber: blockNum, Address: contract.Address}
	var blockKeyBuf bytes.Buffer
	blockKey.EncodeKey(&blockKeyBuf)
	blockKeyBytes := blockKeyBuf.Bytes()

	var storageBytes uint64
	for _, slot := range contract.Storage {
		slotValueU256 := uint256.NewInt(0).SetBytes(slot.Value[:])

		// HashedStorages: keccak(Address) → StorageEntry{keccak(slot_key), slot_value}.
		// Canonical v2 storage table.
		hashedSlotKey := crypto.Keccak256Hash(slot.Key[:])
		hashedEntry := iReth.StorageEntry{Key: hashedSlotKey, Value: slotValueU256}
		var hashedBuf bytes.Buffer
		hashedEntry.EncodeCompact(&hashedBuf)
		hashedEntryBytes := hashedBuf.Bytes()
		if err := txn.Put(envs.MdbxDBIs["HashedStorages"], contract.AddrHash[:], hashedEntryBytes, 0); err != nil {
			return 0, fmt.Errorf("HashedStorages %s slot %s: %w",
				contract.AddrHash.Hex(), slot.Key.Hex(), err)
		}

		if archive {
			// StorageChangeSets: BlockNumberAddress → StorageEntry{slot_key, prev_value=0}
			// For genesis (block 0), the "before" value is 0 — slot was newly set.
			changeEntry := iReth.StorageEntry{Key: slot.Key, Value: uint256.NewInt(0)}
			var changeBuf bytes.Buffer
			changeEntry.EncodeCompact(&changeBuf)
			if err := txn.Put(envs.MdbxDBIs["StorageChangeSets"], blockKeyBytes, changeBuf.Bytes(), 0); err != nil {
				return 0, fmt.Errorf("StorageChangeSets %s slot %s: %w",
					contract.Address.Hex(), slot.Key.Hex(), err)
			}

			// StoragesHistory → RocksDB CF (v2 routing per
			// EitherReader::new_storages_history). Wire format:
			// StorageShardedKey(addr, slot_key, u64::MAX) → EncodeIntegerList([blockNum]).
			if err := envs.HistorySink().PutStorageHistory(contract.Address, slot.Key, blockNum); err != nil {
				return 0, fmt.Errorf("StoragesHistory %s slot %s: %w",
					contract.Address.Hex(), slot.Key.Hex(), err)
			}
		}
		// Stats: bank HashedStorages compact-encoded entry bytes.
		storageBytes += uint64(len(hashedEntryBytes))
	}
	return storageBytes, nil
}
