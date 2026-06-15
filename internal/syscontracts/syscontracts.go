package syscontracts

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
)

// DepositContractAddress is the canonical mainnet Beacon Chain Deposit Contract
// address. Mirrors params.MainnetChainConfig.DepositContractAddress but pinned
// as a package-level constant so callers don't have to thread a ChainConfig.
var DepositContractAddress = common.HexToAddress("0x00000000219ab540356cBB839Cbe05303d7705Fa")

// AddCanonicalSystemContracts injects the four Cancun/Prague pre-execution
// system contracts (BeaconRoots, HistoryStorage, WithdrawalQueue,
// ConsolidationQueue) plus the Beacon Chain Deposit Contract into
// cfg.GenesisAccounts + cfg.GenesisCode. Required for post-Prague block
// processing to match mainnet semantics.
//
// Bytecode sources: params.{BeaconRoots,HistoryStorage,WithdrawalQueue,
// ConsolidationQueue}Code for the Cancun/Prague four; DepositContractCode()
// for the Deposit Contract (see deposit_contract.go for provenance).
//
// Nonce=1 matches go-ethereum's --dev DeveloperGenesisBlock convention.
// Idempotent: calling twice overwrites the same entries.
func AddCanonicalSystemContracts(cfg *generator.Config) {
	if cfg == nil {
		return
	}
	if cfg.GenesisAccounts == nil {
		cfg.GenesisAccounts = map[common.Address]*types.StateAccount{}
	}
	if cfg.GenesisCode == nil {
		cfg.GenesisCode = map[common.Address][]byte{}
	}
	for _, c := range []struct {
		addr common.Address
		code []byte
	}{
		{params.BeaconRootsAddress, params.BeaconRootsCode},
		{params.HistoryStorageAddress, params.HistoryStorageCode},
		{params.WithdrawalQueueAddress, params.WithdrawalQueueCode},
		{params.ConsolidationQueueAddress, params.ConsolidationQueueCode},
		{DepositContractAddress, DepositContractCode()},
	} {
		cfg.GenesisAccounts[c.addr] = &types.StateAccount{
			Nonce:    1,
			Balance:  uint256.NewInt(0),
			Root:     types.EmptyRootHash,
			CodeHash: crypto.Keccak256Hash(c.code).Bytes(),
		}
		cfg.GenesisCode[c.addr] = c.code
	}
}

// Deprecated: use AddCanonicalSystemContracts.
func AddPragueSystemContracts(cfg *generator.Config) {
	AddCanonicalSystemContracts(cfg)
}
