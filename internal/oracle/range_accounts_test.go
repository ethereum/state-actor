package oracle

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
)

func TestAddAccountRange_SequentialAddresses(t *testing.T) {
	cfg := &generator.Config{}
	start := common.HexToAddress("0x0000000000000000000000000000000000001000")
	balance := uint256.NewInt(1_000_000_000_000_000_000) // 1 ETH

	if err := AddAccountRange(cfg, start, 5, balance); err != nil {
		t.Fatalf("AddAccountRange: %v", err)
	}

	if len(cfg.GenesisAccounts) != 5 {
		t.Errorf("got %d accounts, want 5", len(cfg.GenesisAccounts))
	}

	for i := 0; i < 5; i++ {
		addr := common.BigToAddress(new(uint256.Int).Add(
			new(uint256.Int).SetBytes(start[:]), uint256.NewInt(uint64(i)),
		).ToBig())
		acc, ok := cfg.GenesisAccounts[addr]
		if !ok {
			t.Errorf("addr %s missing", addr.Hex())
			continue
		}
		if acc.Nonce != 0 {
			t.Errorf("addr %s: nonce=%d, want 0", addr.Hex(), acc.Nonce)
		}
		if acc.Balance.Cmp(balance) != 0 {
			t.Errorf("addr %s: balance=%s, want %s", addr.Hex(), acc.Balance, balance)
		}
		if acc.Root != types.EmptyRootHash {
			t.Errorf("addr %s: root=%s, want EmptyRootHash", addr.Hex(), acc.Root.Hex())
		}
		if !bytes.Equal(acc.CodeHash, types.EmptyCodeHash.Bytes()) {
			t.Errorf("addr %s: code hash mismatch", addr.Hex())
		}
	}
}

func TestAddAccountRange_FirstAndLastAddresses(t *testing.T) {
	cfg := &generator.Config{}
	start := common.HexToAddress("0x0000000000000000000000000000000000001000")
	if err := AddAccountRange(cfg, start, 150_000, uint256.NewInt(7)); err != nil {
		t.Fatalf("AddAccountRange: %v", err)
	}
	if _, ok := cfg.GenesisAccounts[start]; !ok {
		t.Errorf("start address %s missing", start.Hex())
	}
	// 0x1000 + 150_000 - 1 = 0x1000 + 0x249EF = 0x25_9EF
	wantLast := common.HexToAddress("0x00000000000000000000000000000000000259EF")
	if _, ok := cfg.GenesisAccounts[wantLast]; !ok {
		t.Errorf("last address %s missing", wantLast.Hex())
	}
}

func TestAddAccountRange_DetectsCollision(t *testing.T) {
	cfg := &generator.Config{}
	start := common.HexToAddress("0x0000000000000000000000000000000000001000")
	if err := AddAccountRange(cfg, start, 10, uint256.NewInt(1)); err != nil {
		t.Fatalf("first range: %v", err)
	}
	// Overlapping second range should be rejected.
	overlap := common.HexToAddress("0x0000000000000000000000000000000000001005")
	if err := AddAccountRange(cfg, overlap, 10, uint256.NewInt(1)); err == nil {
		t.Errorf("expected collision error, got nil")
	}
}

func TestAddAccountRange_RejectsOverflow(t *testing.T) {
	cfg := &generator.Config{}
	// Start near the top of the 20-byte space — count of 5 would overflow.
	start := common.HexToAddress("0xfffffffffffffffffffffffffffffffffffffffd")
	if err := AddAccountRange(cfg, start, 5, uint256.NewInt(1)); err == nil {
		t.Errorf("expected overflow error, got nil")
	}
}

func TestAddAccountRange_ZeroCountNoop(t *testing.T) {
	cfg := &generator.Config{}
	if err := AddAccountRange(cfg, common.Address{}, 0, uint256.NewInt(1)); err != nil {
		t.Fatalf("AddAccountRange(count=0): %v", err)
	}
	if len(cfg.GenesisAccounts) != 0 {
		t.Errorf("zero-count call wrote %d accounts", len(cfg.GenesisAccounts))
	}
}
