package syscontracts

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/templates"
)

// expectedSysContracts enumerates the 5 canonical mainnet system contracts
// AddCanonicalSystemContracts must deploy. Addresses + code pulled from
// go-ethereum/params/protocol_params.go (Cancun/Prague pre-execution
// contracts) and oracle.DepositContractCode() (Beacon Chain Deposit
// Contract, vendored from Prysm — see deposit_contract.go).
var expectedSysContracts = []struct {
	name string
	addr common.Address
	code []byte
}{
	{"BeaconRoots (EIP-4788)", params.BeaconRootsAddress, params.BeaconRootsCode},
	{"HistoryStorage (EIP-2935)", params.HistoryStorageAddress, params.HistoryStorageCode},
	{"WithdrawalQueue (EIP-7002)", params.WithdrawalQueueAddress, params.WithdrawalQueueCode},
	{"ConsolidationQueue (EIP-7251)", params.ConsolidationQueueAddress, params.ConsolidationQueueCode},
	{"DepositContract (Beacon Chain)", DepositContractAddress, DepositContractCode()},
}

func TestAddCanonicalSystemContracts_PinsCanonicalAddresses(t *testing.T) {
	// Sanity-check against literal mainnet values per the EIPs — a future
	// params rename or upstream spec drift that silently swaps an address
	// would land here in milliseconds instead of the 5-minute besu boot.
	wantAddrs := map[string]string{
		"BeaconRoots":        "0x000F3df6D732807Ef1319fB7B8bB8522d0Beac02",
		"HistoryStorage":     "0x0000F90827F1C53a10cb7A02335B175320002935",
		"WithdrawalQueue":    "0x00000961Ef480Eb55e80D19ad83579A64c007002",
		"ConsolidationQueue": "0x0000BBdDc7CE488642fb579F8B00f3a590007251",
		"DepositContract":    "0x00000000219ab540356cBB839Cbe05303d7705Fa",
	}
	gotAddrs := map[string]string{
		"BeaconRoots":        params.BeaconRootsAddress.Hex(),
		"HistoryStorage":     params.HistoryStorageAddress.Hex(),
		"WithdrawalQueue":    params.WithdrawalQueueAddress.Hex(),
		"ConsolidationQueue": params.ConsolidationQueueAddress.Hex(),
		"DepositContract":    DepositContractAddress.Hex(),
	}
	for name, want := range wantAddrs {
		if gotAddrs[name] != want {
			t.Errorf("%s address drift: got %s, want %s", name, gotAddrs[name], want)
		}
	}
}

func TestAddCanonicalSystemContracts_DeploysAll5(t *testing.T) {
	cfg := &generator.Config{}
	AddCanonicalSystemContracts(cfg)

	for _, sc := range expectedSysContracts {
		acc, ok := cfg.GenesisAccounts[sc.addr]
		if !ok {
			t.Errorf("%s: GenesisAccounts[%s] missing", sc.name, sc.addr.Hex())
			continue
		}
		if acc.Nonce != 1 {
			t.Errorf("%s: Nonce=%d, want 1 (geth --dev convention)", sc.name, acc.Nonce)
		}
		if acc.Balance == nil || acc.Balance.Sign() != 0 {
			t.Errorf("%s: Balance=%v, want 0", sc.name, acc.Balance)
		}
		if acc.Root != types.EmptyRootHash {
			t.Errorf("%s: Root=%s, want EmptyRootHash (no storage)", sc.name, acc.Root.Hex())
		}
		wantCodeHash := crypto.Keccak256Hash(sc.code).Bytes()
		if !bytes.Equal(acc.CodeHash, wantCodeHash) {
			t.Errorf("%s: CodeHash=%x, want %x", sc.name, acc.CodeHash, wantCodeHash)
		}

		gotCode, ok := cfg.GenesisCode[sc.addr]
		if !ok {
			t.Errorf("%s: GenesisCode[%s] missing", sc.name, sc.addr.Hex())
			continue
		}
		if !bytes.Equal(gotCode, sc.code) {
			t.Errorf("%s: code mismatch (first 32 bytes got=%x want=%x)", sc.name, safePrefix(gotCode, 32), safePrefix(sc.code, 32))
		}
	}

	if len(cfg.GenesisAccounts) != 5 {
		t.Errorf("GenesisAccounts has %d entries, want exactly 5", len(cfg.GenesisAccounts))
	}
	if len(cfg.GenesisCode) != 5 {
		t.Errorf("GenesisCode has %d entries, want exactly 5", len(cfg.GenesisCode))
	}
}

func TestAddCanonicalSystemContracts_Idempotent(t *testing.T) {
	cfg := &generator.Config{}
	AddCanonicalSystemContracts(cfg)
	AddCanonicalSystemContracts(cfg)
	// Calling twice should overwrite (not append duplicates).
	if len(cfg.GenesisAccounts) != 5 {
		t.Errorf("after 2× call: GenesisAccounts has %d entries, want 5", len(cfg.GenesisAccounts))
	}
}

func TestAddCanonicalSystemContracts_PreservesExistingEntries(t *testing.T) {
	otherAddr := common.HexToAddress("0xAbcDef1234567890ABcDeF1234567890aBcDeF12")
	cfg := &generator.Config{
		GenesisAccounts: map[common.Address]*types.StateAccount{
			otherAddr: {Nonce: 42},
		},
	}
	AddCanonicalSystemContracts(cfg)
	if _, ok := cfg.GenesisAccounts[otherAddr]; !ok {
		t.Errorf("existing GenesisAccounts entry dropped")
	}
	if cfg.GenesisAccounts[otherAddr].Nonce != 42 {
		t.Errorf("existing GenesisAccounts entry mutated")
	}
}

func TestAddCanonicalSystemContracts_NilCfgSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AddCanonicalSystemContracts(nil) panicked: %v", r)
		}
	}()
	AddCanonicalSystemContracts(nil)
}

// TestAddPragueSystemContracts_DeprecatedAlias pins that the legacy entry
// point still delegates to AddCanonicalSystemContracts so any in-flight
// caller that hasn't migrated yet still gets the full 5-contract set.
func TestAddPragueSystemContracts_DeprecatedAlias(t *testing.T) {
	cfg := &generator.Config{}
	AddPragueSystemContracts(cfg)
	if len(cfg.GenesisAccounts) != 5 {
		t.Errorf("AddPragueSystemContracts (deprecated alias) populated %d entries, want 5 (must delegate to canonical set)", len(cfg.GenesisAccounts))
	}
	if _, ok := cfg.GenesisAccounts[DepositContractAddress]; !ok {
		t.Errorf("AddPragueSystemContracts (deprecated alias) missed the Deposit Contract")
	}
}

func safePrefix(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func TestAddAmsterdamSystemContracts_DeploysAll3(t *testing.T) {
	cfg := &generator.Config{}
	AddCanonicalSystemContracts(cfg)
	AddAmsterdamSystemContracts(cfg)

	want := []struct {
		name string
		addr string
		code []byte
	}{
		{"BuilderDeposit (EIP-8282)", "0x0000BFF46984E3725691FA540A8C7589300D8282", params.BuilderDepositCode},
		{"BuilderExit (EIP-8282)", "0x000064D678505AD48F8CCB093BC65613800E8282", params.BuilderExitCode},
		{"DeterministicFactory (EIP-7997)", "0x4e59b44847b379578588920cA78FbF26c0B4956C", params.DeterministicFactoryCode},
	}
	for _, sc := range want {
		addr := common.HexToAddress(sc.addr)
		acc, ok := cfg.GenesisAccounts[addr]
		if !ok {
			t.Errorf("%s: GenesisAccounts[%s] missing", sc.name, sc.addr)
			continue
		}
		if acc.Nonce != 1 || acc.Balance.Sign() != 0 || acc.Root != types.EmptyRootHash {
			t.Errorf("%s: got nonce=%d balance=%v root=%s, want 1/0/EmptyRootHash",
				sc.name, acc.Nonce, acc.Balance, acc.Root.Hex())
		}
		if !bytes.Equal(acc.CodeHash, crypto.Keccak256(sc.code)) {
			t.Errorf("%s: CodeHash does not match the bytecode", sc.name)
		}
		if !bytes.Equal(cfg.GenesisCode[addr], sc.code) {
			t.Errorf("%s: GenesisCode mismatch", sc.name)
		}
	}
	if len(cfg.GenesisAccounts) != 8 {
		t.Errorf("GenesisAccounts has %d entries, want 5 canonical + 3 Amsterdam", len(cfg.GenesisAccounts))
	}
}

// TestAddAmsterdamSystemContracts_SpecFactoryWins pins that a spec entity
// at the EIP-7997 factory address (the create2_factory template) is not
// duplicated: generator.Config.Validate rejects two accounts at one address.
func TestAddAmsterdamSystemContracts_SpecFactoryWins(t *testing.T) {
	cfg := &generator.Config{
		PreAlloc: []templates.PreAllocEntity{{
			Address: params.DeterministicFactoryAddress,
			Account: &types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0)},
			Code:    params.DeterministicFactoryCode,
		}},
	}
	AddAmsterdamSystemContracts(cfg)
	if _, ok := cfg.GenesisAccounts[params.DeterministicFactoryAddress]; ok {
		t.Error("factory added to GenesisAccounts although PreAlloc declares it")
	}
	if _, ok := cfg.GenesisAccounts[params.BuilderDepositAddress]; !ok {
		t.Error("BuilderDeposit missing")
	}
}
