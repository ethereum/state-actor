package reth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

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
