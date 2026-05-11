package oracle

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"

	"github.com/nerolation/state-actor/generator"
)

// expectedSysContracts enumerates the 4 EIP system contracts
// AddPragueSystemContracts must deploy. Address + code pulled from
// go-ethereum/params/protocol_params.go — the canonical source of
// truth for EIP-4788/2935/7002/7251 system contracts.
var expectedSysContracts = []struct {
	name string
	addr common.Address
	code []byte
}{
	{"BeaconRoots (EIP-4788)", params.BeaconRootsAddress, params.BeaconRootsCode},
	{"HistoryStorage (EIP-2935)", params.HistoryStorageAddress, params.HistoryStorageCode},
	{"WithdrawalQueue (EIP-7002)", params.WithdrawalQueueAddress, params.WithdrawalQueueCode},
	{"ConsolidationQueue (EIP-7251)", params.ConsolidationQueueAddress, params.ConsolidationQueueCode},
}

func TestAddPragueSystemContracts_PinsCanonicalAddresses(t *testing.T) {
	// Sanity-check against literal mainnet values per the EIPs — a future
	// params rename or upstream spec drift that silently swaps an address
	// would land here in milliseconds instead of the 5-minute besu boot.
	wantAddrs := map[string]string{
		"BeaconRoots":        "0x000F3df6D732807Ef1319fB7B8bB8522d0Beac02",
		"HistoryStorage":     "0x0000F90827F1C53a10cb7A02335B175320002935",
		"WithdrawalQueue":    "0x00000961Ef480Eb55e80D19ad83579A64c007002",
		"ConsolidationQueue": "0x0000BBdDc7CE488642fb579F8B00f3a590007251",
	}
	gotAddrs := map[string]string{
		"BeaconRoots":        params.BeaconRootsAddress.Hex(),
		"HistoryStorage":     params.HistoryStorageAddress.Hex(),
		"WithdrawalQueue":    params.WithdrawalQueueAddress.Hex(),
		"ConsolidationQueue": params.ConsolidationQueueAddress.Hex(),
	}
	for name, want := range wantAddrs {
		if gotAddrs[name] != want {
			t.Errorf("%s address drift: got %s, want %s", name, gotAddrs[name], want)
		}
	}
}

func TestAddPragueSystemContracts_DeploysAll4(t *testing.T) {
	cfg := &generator.Config{}
	AddPragueSystemContracts(cfg)

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

	if len(cfg.GenesisAccounts) != 4 {
		t.Errorf("GenesisAccounts has %d entries, want exactly 4", len(cfg.GenesisAccounts))
	}
	if len(cfg.GenesisCode) != 4 {
		t.Errorf("GenesisCode has %d entries, want exactly 4", len(cfg.GenesisCode))
	}
}

func TestAddPragueSystemContracts_Idempotent(t *testing.T) {
	cfg := &generator.Config{}
	AddPragueSystemContracts(cfg)
	AddPragueSystemContracts(cfg)
	// Calling twice should overwrite (not append duplicates).
	if len(cfg.GenesisAccounts) != 4 {
		t.Errorf("after 2× call: GenesisAccounts has %d entries, want 4", len(cfg.GenesisAccounts))
	}
}

func TestAddPragueSystemContracts_PreservesExistingEntries(t *testing.T) {
	otherAddr := common.HexToAddress("0xAbcDef1234567890ABcDeF1234567890aBcDeF12")
	cfg := &generator.Config{
		GenesisAccounts: map[common.Address]*types.StateAccount{
			otherAddr: {Nonce: 42},
		},
	}
	AddPragueSystemContracts(cfg)
	if _, ok := cfg.GenesisAccounts[otherAddr]; !ok {
		t.Errorf("existing GenesisAccounts entry dropped")
	}
	if cfg.GenesisAccounts[otherAddr].Nonce != 42 {
		t.Errorf("existing GenesisAccounts entry mutated")
	}
}

func TestAddPragueSystemContracts_NilCfgSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AddPragueSystemContracts(nil) panicked: %v", r)
		}
	}()
	AddPragueSystemContracts(nil)
}
