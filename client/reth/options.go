package reth

import (
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
)

// Options carries optional knobs for RunCgo. Reserved for future use; the
// zero value is the supported default.
type Options struct {
	// (No options exposed yet. Future candidates: bytecode-LRU capacity,
	// commit-threshold override, scratch directory, etc.)
}

// buildInjectedAccount returns an entitygen.Account for a pre-funded EOA at
// the supplied address. Used by RunCgo to honour cfg.InjectAddresses (e.g.
// the Anvil dev account 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266).
//
// Balance: 999_999_999 ETH (same as the legacy JSONL path so spamoor and
// other test harnesses keep working). Nonce 0, no code, no storage.
func buildInjectedAccount(addr common.Address) *entitygen.Account {
	balance := new(uint256.Int).Mul(
		uint256.NewInt(999_999_999),
		uint256.NewInt(1e18),
	)
	return &entitygen.Account{
		Address:  addr,
		AddrHash: crypto.Keccak256Hash(addr[:]),
		StateAccount: &types.StateAccount{
			Nonce:    0,
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
	}
}

// buildAllocAccounts converts cfg.GenesisAccounts/Code/Storage into a
// slice of *entitygen.Account suitable for reth's WriteContracts path.
// WriteContracts mutates each Account's StateAccount.Root + .CodeHash
// from the supplied Storage + Code, so the per-account RLP captures the
// correct global-state-trie values.
//
// Iteration order is sorted by address so the resulting state root is
// deterministic across runs.
func buildAllocAccounts(cfg generator.Config) []*entitygen.Account {
	addrs := make([]common.Address, 0, len(cfg.GenesisAccounts))
	for a := range cfg.GenesisAccounts {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].Hex() < addrs[j].Hex()
	})

	out := make([]*entitygen.Account, 0, len(addrs))
	for _, addr := range addrs {
		acc := cfg.GenesisAccounts[addr]
		code := cfg.GenesisCode[addr]
		var slots []entitygen.StorageSlot
		if stor := cfg.GenesisStorage[addr]; len(stor) > 0 {
			slots = make([]entitygen.StorageSlot, 0, len(stor))
			for k, v := range stor {
				slots = append(slots, entitygen.StorageSlot{Key: k, Value: v})
			}
			sort.Slice(slots, func(i, j int) bool {
				return slots[i].Key.Hex() < slots[j].Key.Hex()
			})
		}
		sa := *acc // copy — don't mutate the caller's map
		out = append(out, &entitygen.Account{
			Address:      addr,
			AddrHash:     crypto.Keccak256Hash(addr[:]),
			StateAccount: &sa,
			Code:         code,
			Storage:      slots,
		})
	}
	return out
}
