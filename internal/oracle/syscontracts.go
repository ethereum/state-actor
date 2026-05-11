package oracle

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
)

// AddPragueSystemContracts populates cfg.GenesisAccounts + cfg.GenesisCode
// with the EIP-4788 (Cancun) and EIPs 7002/7251/2935 (Prague) system
// contracts at their canonical addresses. Without these, post-Prague
// block processing fails on every block:
//
//   - besu: "Failed to start Besu: Withdrawal Request Contract Address
//     not found" at boot, then "Invalid system call address" + "Block
//     creation failed unexpectedly" on every engine_forkchoiceUpdated.
//   - geth/reth: silently no-ops the system call (works for tests
//     today; correctness diverges from real Prague semantics).
//
// Bytecodes are pulled from go-ethereum/params/protocol_params.go which
// owns the canonical wire-form (3373fffffffffffffffffffffffffffffffffffffffe…
// for each, encoding the system-call ABI). Addresses match
// params.BeaconRootsAddress / HistoryStorageAddress /
// WithdrawalQueueAddress / ConsolidationQueueAddress.
//
// The DepositContract (0x00000000219ab54…) is NOT deployed here —
// state-actor's e2e suite never makes deposit txs, and besu doesn't
// pre-execute the deposit contract per block (only on inbound deposit
// txs). Mainnet deploys a real ~16KB ABI contract there; we'd need to
// vendor that bytecode separately.
//
// Writes go through cfg.GenesisAccounts + cfg.GenesisCode (NOT
// g.Alloc) — that's the field each client writer reads when composing
// the genesis state. StateAccount is constructed with the correct
// Root (EmptyRootHash — no storage) + CodeHash (keccak256(code)) so
// geth/nethermind/reth writers (which encode the account RLP from the
// StateAccount struct verbatim) produce the correct state root. Besu's
// writer reconstructs both from code separately, so it tolerates zero
// values too, but uniform field values keep all 4 writers in lock-step.
//
// Nonce=1 matches geth's `--dev` `DeveloperGenesisBlock` convention
// (go-ethereum/core/genesis.go) so a chain built by state-actor and a
// chain built by `geth --dev` from scratch produce the same state for
// the system contracts.
//
// Called from each client's e2e_test.go AND each client's golden_test.go
// right after BuildSynthetic. The canonical cross-client invariant is
// "Osaka-bootable state" — synthetic entitygen entities + InjectAddresses
// + the 4 system contracts (this helper).
func AddPragueSystemContracts(cfg *generator.Config) {
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
