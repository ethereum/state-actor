package oracle

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
)

// AddAccountRange writes `count` pure EOAs into cfg.GenesisAccounts at
// sequential addresses [start, start+count), each funded with `balance`.
//
// Nonce=0, no code, no storage. Used to bloat the state with a known,
// addressable range — benchmark tests target addresses in this range
// directly without needing to read back generated state.
//
// Returns an error if the range overflows 20 bytes or collides with an
// address already in cfg.GenesisAccounts. Caller should call this before
// the generator runs (the generator-produced randoms are 20 bytes wide
// and statistically won't hit a contiguous range, but the explicit
// collision check catches double-injection of overlapping ranges).
func AddAccountRange(cfg *generator.Config, start common.Address, count int, balance *uint256.Int) error {
	if cfg == nil {
		return fmt.Errorf("AddAccountRange: nil cfg")
	}
	if count < 0 {
		return fmt.Errorf("AddAccountRange: negative count %d", count)
	}
	if count == 0 {
		return nil
	}
	if balance == nil {
		balance = uint256.NewInt(0)
	}

	// Address space is 160 bits. Treat start as a big-endian integer and
	// detect overflow by checking that start+count-1 still fits in 20 bytes.
	startInt := new(big.Int).SetBytes(start[:])
	end := new(big.Int).Add(startInt, big.NewInt(int64(count-1)))
	if end.BitLen() > 160 {
		return fmt.Errorf("AddAccountRange: range overflows 20 bytes (start=%s, count=%d)", start.Hex(), count)
	}

	if cfg.GenesisAccounts == nil {
		cfg.GenesisAccounts = map[common.Address]*types.StateAccount{}
	}

	cur := new(big.Int).Set(startInt)
	for i := 0; i < count; i++ {
		var addr common.Address
		curBytes := cur.Bytes()
		copy(addr[20-len(curBytes):], curBytes)

		if _, dup := cfg.GenesisAccounts[addr]; dup {
			return fmt.Errorf("AddAccountRange: address %s already in GenesisAccounts (range %s..%s, count %d)",
				addr.Hex(), start.Hex(), end.Text(16), count)
		}

		cfg.GenesisAccounts[addr] = &types.StateAccount{
			Nonce:    0,
			Balance:  new(uint256.Int).Set(balance),
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}

		cur.Add(cur, big.NewInt(1))
	}

	return nil
}
