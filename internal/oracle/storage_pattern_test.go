package oracle

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
)

func TestAddStoragePatternAccount_LaysSlotsCorrectly(t *testing.T) {
	cfg := &generator.Config{}
	target := common.HexToAddress("0xCafeCafeCafeCafeCafeCafeCafeCafeCafeCafe")
	if err := AddStoragePatternAccount(cfg, target, 10, 1, uint256.NewInt(0)); err != nil {
		t.Fatalf("AddStoragePatternAccount: %v", err)
	}

	acc, ok := cfg.GenesisAccounts[target]
	if !ok {
		t.Fatalf("target %s missing from GenesisAccounts", target.Hex())
	}
	if acc.Nonce != 1 {
		t.Errorf("nonce=%d, want 1", acc.Nonce)
	}
	if acc.Root != types.EmptyRootHash {
		t.Errorf("root=%s, want EmptyRootHash placeholder", acc.Root.Hex())
	}
	if !bytes.Equal(acc.CodeHash, types.EmptyCodeHash.Bytes()) {
		t.Errorf("code hash mismatch")
	}

	stor := cfg.GenesisStorage[target]
	if len(stor) != 11 {
		t.Errorf("storage size=%d, want 11 (slot 0 + slots 1..10)", len(stor))
	}
	// slot 0 = final + 1 = 11
	if got := stor[common.Hash{}]; got != uint64ToHash(11) {
		t.Errorf("slot 0 = %s, want %s", got.Hex(), uint64ToHash(11).Hex())
	}
	// slot k = k for k in 1..10
	for k := uint64(1); k <= 10; k++ {
		got := stor[uint64ToHash(k)]
		if got != uint64ToHash(k) {
			t.Errorf("slot %d = %s, want %s", k, got.Hex(), uint64ToHash(k).Hex())
		}
	}
}

func TestAddStoragePatternAccount_RejectsNonceZero(t *testing.T) {
	cfg := &generator.Config{}
	target := common.HexToAddress("0xCafeCafeCafeCafeCafeCafeCafeCafeCafeCafe")
	if err := AddStoragePatternAccount(cfg, target, 5, 0, uint256.NewInt(0)); err == nil {
		t.Errorf("expected nonce=0 rejection, got nil")
	}
}

func TestAddStoragePatternAccount_RejectsCollision(t *testing.T) {
	cfg := &generator.Config{}
	target := common.HexToAddress("0xCafeCafeCafeCafeCafeCafeCafeCafeCafeCafe")
	if err := AddStoragePatternAccount(cfg, target, 5, 1, uint256.NewInt(0)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := AddStoragePatternAccount(cfg, target, 5, 1, uint256.NewInt(0)); err == nil {
		t.Errorf("expected collision error, got nil")
	}
}

func TestAddStoragePatternAccount_FinalZero(t *testing.T) {
	cfg := &generator.Config{}
	target := common.HexToAddress("0xCafeCafeCafeCafeCafeCafeCafeCafeCafeCafe")
	if err := AddStoragePatternAccount(cfg, target, 0, 1, uint256.NewInt(0)); err != nil {
		t.Fatalf("final=0: %v", err)
	}
	stor := cfg.GenesisStorage[target]
	if len(stor) != 1 {
		t.Errorf("final=0: storage size=%d, want 1 (just slot 0)", len(stor))
	}
	// slot 0 = 0 + 1 = 1
	if got := stor[common.Hash{}]; got != uint64ToHash(1) {
		t.Errorf("slot 0 = %s, want %s", got.Hex(), uint64ToHash(1).Hex())
	}
}

func TestUint64ToHash_BigEndian(t *testing.T) {
	got := uint64ToHash(0x1234)
	want := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000001234")
	if got != want {
		t.Errorf("uint64ToHash(0x1234) = %s, want %s", got.Hex(), want.Hex())
	}
}
