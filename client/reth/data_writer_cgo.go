//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"

	"github.com/erigontech/mdbx-go/mdbx"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// WriteEOAs writes the v2 data-table rows for each account inside one
// atomic MDBX write transaction.
//
// Tables written per EOA:
//   - HashedAccounts (keccak(Address) → Account) — canonical v2 state, always.
//   - AccountChangeSets (DupSort: BE_u64(block) → AccountBeforeTx{addr, nil}) — archive only.
//   - AccountsHistory (ShardedKey(addr, u64::MAX) → IntegerList([blockNum])) — archive only;
//     routed to the RocksDB CF via envs.HistorySink().
//
// Accounts are written in input order (caller controls ordering). Uses
// tx.Put (not cursor.Append) for safety regardless of input ordering.
//
// stats (optional) accumulates AccountBytes — the encoded compact-Account
// size for every account written. Pass nil to skip accounting; only applied
// after the MDBX transaction commits, so a rollback leaves stats untouched.
func WriteEOAs(envs *Envs, accounts []*entitygen.Account, blockNum uint64, archive bool, stats *generator.Stats) error {
	var localAccountBytes uint64
	err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		blockKey := beU64(blockNum)

		for _, acc := range accounts {
			if acc.StateAccount == nil {
				return fmt.Errorf("WriteEOAs: account %s has nil StateAccount", acc.Address.Hex())
			}

			ethAccount := iReth.Account{
				Nonce:        acc.StateAccount.Nonce,
				Balance:      acc.StateAccount.Balance, // *uint256.Int
				BytecodeHash: nil,                      // EOA: no code
			}
			var accBuf bytes.Buffer
			ethAccount.EncodeCompact(&accBuf)
			accountBytes := accBuf.Bytes()

			// HashedAccounts — keccak(addr) → Account. Canonical v2 state;
			// reth's Latest/HistoricalStateProvider reads this table.
			if err := txn.Put(envs.MdbxDBIs["HashedAccounts"], acc.AddrHash[:], accountBytes, 0); err != nil {
				return fmt.Errorf("HashedAccounts %s: %w", acc.Address.Hex(), err)
			}

			if archive {
				// AccountChangeSets — DupSort: BE_u64(block) → AccountBeforeTx{addr, nil}
				// Address is the DupSort SubKey (encoded first in AccountBeforeTx.EncodeCompact).
				// Info=nil: account had no prior state (genesis creation).
				abt := iReth.AccountBeforeTx{Address: acc.Address, Info: nil}
				var abtBuf bytes.Buffer
				abt.EncodeCompact(&abtBuf)
				if err := txn.Put(envs.MdbxDBIs["AccountChangeSets"], blockKey[:], abtBuf.Bytes(), 0); err != nil {
					return fmt.Errorf("AccountChangeSets %s: %w", acc.Address.Hex(), err)
				}

				// AccountsHistory → RocksDB CF (v2 routing per
				// EitherReader::new_accounts_history). Wire format:
				// ShardedKeyAddress(addr, u64::MAX) → EncodeIntegerList([blockNum]).
				if err := envs.HistorySink().PutAccountHistory(acc.Address, blockNum); err != nil {
					return fmt.Errorf("AccountsHistory %s: %w", acc.Address.Hex(), err)
				}
			}

			// Bank the row's bytes locally; transferred to stats only if
			// Update succeeds.
			localAccountBytes += uint64(len(accountBytes))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if stats != nil {
		stats.AccountBytes += localAccountBytes
	}
	return nil
}
