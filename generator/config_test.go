package generator

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestConfig_Validate_HappyPath(t *testing.T) {
	addr := common.HexToAddress("0x2222222222222222222222222222222222222222")

	cfg := Config{
		GenesisAccounts: map[common.Address]*types.StateAccount{addr: {}},
		GenesisCode:     map[common.Address][]byte{addr: {0x01, 0x02}},
		GenesisStorage:  map[common.Address]map[common.Hash]common.Hash{addr: {{0x1}: {0x2}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
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
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject a config with no AutoFill / PreAlloc / GenesisAccounts")
	}
	if !strings.Contains(err.Error(), "no entities to emit") {
		t.Errorf("error should mention missing entities, got: %v", err)
	}
}
