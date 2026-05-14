package oracle

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
)

// AddStoragePatternAccount writes a self-pointer + dense counter storage
// pattern onto `target`:
//
//	slot 0          = final + 1   (pointer to "next free index")
//	slot k (1..N)   = k           (identity)
//
// The account is created with nonce >= 1 (per spec — empty-account
// pruning would otherwise wipe a code-less, balance-0 account on
// Spurious Dragon+ chains) and the configured balance. No code.
//
// Returns an error if the target already has storage or an account
// entry in cfg.GenesisAccounts, or if final < 0.
func AddStoragePatternAccount(cfg *generator.Config, target common.Address, final int, nonce uint64, balance *uint256.Int) error {
	if cfg == nil {
		return fmt.Errorf("AddStoragePatternAccount: nil cfg")
	}
	if final < 0 {
		return fmt.Errorf("AddStoragePatternAccount: negative final %d", final)
	}
	if nonce == 0 {
		return fmt.Errorf("AddStoragePatternAccount: nonce must be >= 1 (got 0); empty-account pruning would wipe this entry")
	}
	if balance == nil {
		balance = uint256.NewInt(0)
	}

	if _, dup := cfg.GenesisAccounts[target]; dup {
		return fmt.Errorf("AddStoragePatternAccount: target %s already in GenesisAccounts", target.Hex())
	}
	if _, dup := cfg.GenesisStorage[target]; dup {
		return fmt.Errorf("AddStoragePatternAccount: target %s already in GenesisStorage", target.Hex())
	}

	if cfg.GenesisAccounts == nil {
		cfg.GenesisAccounts = map[common.Address]*types.StateAccount{}
	}
	if cfg.GenesisStorage == nil {
		cfg.GenesisStorage = map[common.Address]map[common.Hash]common.Hash{}
	}

	storage := make(map[common.Hash]common.Hash, final+1)
	// slot 0 = final + 1
	storage[common.Hash{}] = uint64ToHash(uint64(final) + 1)
	// slot k = k for k in 1..final
	for k := 1; k <= final; k++ {
		storage[uint64ToHash(uint64(k))] = uint64ToHash(uint64(k))
	}
	cfg.GenesisStorage[target] = storage

	// Storage root is computed by each client writer from
	// cfg.GenesisStorage[target]; we set Root to EmptyRootHash here
	// because it's a placeholder — writers replace it with the
	// keccak-of-the-storage-trie value before the account RLP is
	// finalized. See e.g. client/reth/options.go:67 (buildAllocAccounts)
	// which calls storageRootFromAlloc(stor).
	cfg.GenesisAccounts[target] = &types.StateAccount{
		Nonce:    nonce,
		Balance:  new(uint256.Int).Set(balance),
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}

	return nil
}

func uint64ToHash(v uint64) common.Hash {
	var h common.Hash
	// Right-align 8 bytes of big-endian v into the 32-byte hash.
	for i := 0; i < 8; i++ {
		h[31-i] = byte(v >> (8 * i))
	}
	return h
}

