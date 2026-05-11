package generator

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestConfig_Validate_HappyPath(t *testing.T) {
	addrA := common.HexToAddress("0x1111111111111111111111111111111111111111")
	addrB := common.HexToAddress("0x2222222222222222222222222222222222222222")

	cfg := Config{
		InjectAddresses: []common.Address{addrA},
		GenesisAccounts: map[common.Address]*types.StateAccount{addrB: {}},
		GenesisCode:     map[common.Address][]byte{addrB: {0x01, 0x02}},
		GenesisStorage:  map[common.Address]map[common.Hash]common.Hash{addrB: {{0x1}: {0x2}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestConfig_Validate_RejectsInjectGenesisCollision(t *testing.T) {
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	cfg := Config{
		InjectAddresses: []common.Address{addr},
		GenesisAccounts: map[common.Address]*types.StateAccount{addr: {}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject InjectAddresses ∩ GenesisAccounts collision")
	}
	if !strings.Contains(err.Error(), "ambiguous precedence") {
		t.Errorf("error should mention precedence ambiguity, got: %v", err)
	}
	if !strings.Contains(err.Error(), addr.Hex()) && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(addr.Hex())) {
		t.Errorf("error should name the offending address %s, got: %v", addr.Hex(), err)
	}
}

func TestConfig_Validate_RejectsOrphanCode(t *testing.T) {
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	cfg := Config{
		GenesisCode: map[common.Address][]byte{addr: {0x01}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject orphan GenesisCode")
	}
	if !strings.Contains(err.Error(), "orphan code") {
		t.Errorf("error should mention orphan code, got: %v", err)
	}
}

func TestConfig_Validate_RejectsOrphanStorage(t *testing.T) {
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	cfg := Config{
		GenesisStorage: map[common.Address]map[common.Hash]common.Hash{addr: {{0x1}: {0x2}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject orphan GenesisStorage")
	}
	if !strings.Contains(err.Error(), "orphan storage") {
		t.Errorf("error should mention orphan storage, got: %v", err)
	}
}

func TestConfig_Validate_EmptyConfig(t *testing.T) {
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate should accept empty config: %v", err)
	}
}
