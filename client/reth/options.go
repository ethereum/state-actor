package reth

import (
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/entitygen"
)

// Options carries optional knobs for RunCgo. The zero value is the
// supported default (fresh fill profile, dense everywhere).
type Options struct {
	// FillProfileName is the resolved --mdbx-fill-profile name, recorded
	// verbatim into the run manifest — RunCgo does not substitute "fresh"
	// for an empty string. main.go's flag defaults to "fresh" and always
	// passes it, so callers other than tests constructing a zero Options
	// never hit that case.
	FillProfileName string
	// FillTargets maps MDBX table name (see FillTables) -> target leaf fill
	// in (0.5, 1.0], resolved from FillProfileName + --mdbx-fill overrides
	// via ResolveFillProfile. A table absent from the map stays dense
	// (split_reserve=0). nil/empty means dense everywhere.
	FillTargets map[string]float64
}

// buildAllocAccounts converts cfg.GenesisAccounts/Code/Storage into a
// slice of *entitygen.Account suitable for reth's WriteContracts path.
// Code comes from cfg.GenesisCode only: lazy code stays out, so the
// caller makes it with cfg.CodeAt one batch at a time.
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
