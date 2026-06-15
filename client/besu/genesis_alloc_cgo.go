//go:build cgo_besu

package besu

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/besu"
	besurlp "github.com/ethereum/state-actor/internal/besu/rlp"
	besutrie "github.com/ethereum/state-actor/internal/besu/trie"
)

// writeGenesisAllocAccounts feeds a genesis JSON's `alloc` map directly into
// Phase 2 (skipping synthetic entity generation). Used by the differential
// oracle to compare our writer's state root against Besu's pinned values.
func writeGenesisAllocAccounts(
	ctx context.Context,
	db *besuDB,
	sink *nodeSink,
	allocs map[common.Address]genesis.GenesisAccount,
) (common.Hash, []byte, error) {
	// Sort by keccak256(addr).
	type sortedEntry struct {
		addr     common.Address
		addrHash common.Hash
		acc      genesis.GenesisAccount
	}
	entries := make([]sortedEntry, 0, len(allocs))
	for addr, acc := range allocs {
		entries = append(entries, sortedEntry{
			addr:     addr,
			addrHash: crypto.Keccak256Hash(addr[:]),
			acc:      acc,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].addrHash[:], entries[j].addrHash[:]) < 0
	})

	builder := besutrie.New(sink)

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, nil, err
		}

		storageRoot := besu.EmptyTrieNodeHash
		codeHash := besu.EmptyCodeHash

		// Per-account storage trie via the streaming builder (O(depth) memory).
		if len(e.acc.Storage) > 0 {
			sb := builder.BeginStreamingStorage(e.addrHash)
			// Sort slots by slotHash — the streaming builder requires
			// keccak-ascending input.
			type slotKV struct {
				slotKey  common.Hash
				slotHash common.Hash
				value    common.Hash
			}
			slots := make([]slotKV, 0, len(e.acc.Storage))
			for k, v := range e.acc.Storage {
				slots = append(slots, slotKV{
					slotKey:  k,
					slotHash: crypto.Keccak256Hash(k[:]),
					value:    v,
				})
			}
			sort.Slice(slots, func(i, j int) bool {
				return bytes.Compare(slots[i].slotHash[:], slots[j].slotHash[:]) < 0
			})
			for _, s := range slots {
				valueRLP := besurlp.EncodeStorageValue(s.value)
				if err := sb.AddSlot(s.slotHash, valueRLP); err != nil {
					return common.Hash{}, nil, err
				}
				if err := sink.PutFlatStorage(e.addrHash, s.slotHash, valueRLP); err != nil {
					return common.Hash{}, nil, err
				}
			}
			root, err := sb.Commit()
			if err != nil {
				return common.Hash{}, nil, err
			}
			storageRoot = root
		}

		// Code.
		code := []byte(e.acc.Code)
		if len(code) > 0 {
			codeHash = crypto.Keccak256Hash(code)
			if err := sink.PutCode(codeHash, code); err != nil {
				return common.Hash{}, nil, err
			}
		}

		// Account RLP.
		balance := uint256.NewInt(0)
		if e.acc.Balance != nil {
			// uint256.FromBig returns (value, overflowed). The bool is true
			// only when the input exceeds 2^256-1 — NOT a success indicator.
			b, overflow := uint256.FromBig(e.acc.Balance.ToInt())
			if overflow {
				return common.Hash{}, nil, fmt.Errorf("besu: balance overflow (>2^256) for %s", e.addr.Hex())
			}
			balance = b
		}
		accountRLP, err := besurlp.EncodeAccount(uint64(e.acc.Nonce), balance, storageRoot, codeHash)
		if err != nil {
			return common.Hash{}, nil, fmt.Errorf("besu: encode account %s: %w", e.addr.Hex(), err)
		}

		if err := sink.PutFlatAccount(e.addrHash, accountRLP); err != nil {
			return common.Hash{}, nil, err
		}
		if err := builder.AddAccount(e.addrHash, accountRLP); err != nil {
			return common.Hash{}, nil, err
		}
	}

	return builder.Commit()
}
