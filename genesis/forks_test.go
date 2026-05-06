package genesis

import (
	"math/big"
	"testing"
)

func TestBuildChainConfigForFork_PragueActive(t *testing.T) {
	cfg, err := BuildChainConfigForFork("prague", big.NewInt(1337))
	if err != nil {
		t.Fatalf("BuildChainConfigForFork: %v", err)
	}
	if cfg.ChainID == nil || cfg.ChainID.Sign() == 0 || cfg.ChainID.Int64() != 1337 {
		t.Fatalf("chainID = %v, want 1337", cfg.ChainID)
	}

	zero := big.NewInt(0)
	if !cfg.IsHomestead(zero) {
		t.Error("Homestead should be active at block 0")
	}
	if !cfg.IsLondon(zero) {
		t.Error("London should be active at block 0")
	}
	if !cfg.IsShanghai(zero, 0) {
		t.Error("Shanghai should be active at time 0")
	}
	if !cfg.IsCancun(zero, 0) {
		t.Error("Cancun should be active at time 0")
	}
	if !cfg.IsPrague(zero, 0) {
		t.Error("Prague should be active at time 0")
	}
	if cfg.IsOsaka(zero, 0) {
		t.Error("Osaka should NOT be active when --fork=prague")
	}
}

func TestBuildChainConfigForFork_ShanghaiOnlyActivatesShanghai(t *testing.T) {
	cfg, err := BuildChainConfigForFork("shanghai", big.NewInt(1))
	if err != nil {
		t.Fatalf("BuildChainConfigForFork: %v", err)
	}
	zero := big.NewInt(0)
	if !cfg.IsLondon(zero) {
		t.Error("London should still be active for shanghai (earlier fork)")
	}
	if !cfg.IsShanghai(zero, 0) {
		t.Error("Shanghai should be active")
	}
	if cfg.IsCancun(zero, 0) {
		t.Error("Cancun must NOT be active when --fork=shanghai")
	}
}

func TestBuildChainConfigForFork_AliasesResolveToDefault(t *testing.T) {
	for _, alias := range []string{"", "latest", "default", "LATEST", "  prague  "} {
		cfg, err := BuildChainConfigForFork(alias, big.NewInt(1))
		if err != nil {
			t.Fatalf("alias %q: %v", alias, err)
		}
		if !cfg.IsPrague(big.NewInt(0), 0) {
			t.Errorf("alias %q: Prague should be active (DefaultFork=%q)", alias, DefaultFork)
		}
	}
}

func TestBuildChainConfigForFork_UnknownReturnsError(t *testing.T) {
	if _, err := BuildChainConfigForFork("frontier-but-spelled-wrong", big.NewInt(1)); err == nil {
		t.Fatal("expected error for unknown fork name")
	}
	if _, err := BuildChainConfigForFork("prague", nil); err == nil {
		t.Fatal("expected error for nil chainID")
	}
}

func TestListForks_ContainsCanonicalNames(t *testing.T) {
	got := ListForks()
	want := map[string]bool{
		"homestead": true, "london": true, "merge": true,
		"shanghai": true, "cancun": true, "prague": true, "osaka": true,
	}
	have := make(map[string]bool, len(got))
	for _, n := range got {
		have[n] = true
	}
	for w := range want {
		if !have[w] {
			t.Errorf("ListForks() missing %q", w)
		}
	}
}

func TestLatestForkName_StableAndKnown(t *testing.T) {
	name := LatestForkName()
	if name != DefaultFork {
		t.Errorf("LatestForkName() = %q, want DefaultFork=%q", name, DefaultFork)
	}
	cfg, err := BuildChainConfigForFork(name, big.NewInt(1))
	if err != nil {
		t.Fatalf("LatestForkName() = %q is not buildable: %v", name, err)
	}
	if !cfg.IsPrague(big.NewInt(0), 0) {
		t.Errorf("LatestForkName() = %q does not activate Prague", name)
	}
}

func TestBuildSynthetic_PrimitivesPlumbed(t *testing.T) {
	g, err := BuildSynthetic("prague", big.NewInt(7), 25_000_000, 999, []byte{0xde, 0xad})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	if g.Config == nil || g.Config.ChainID.Int64() != 7 {
		t.Errorf("ChainID lost: %v", g.Config)
	}
	if uint64(g.GasLimit) != 25_000_000 {
		t.Errorf("GasLimit = %d, want 25_000_000", g.GasLimit)
	}
	if uint64(g.Timestamp) != 999 {
		t.Errorf("Timestamp = %d, want 999", g.Timestamp)
	}
	if len(g.ExtraData) != 2 || g.ExtraData[0] != 0xde || g.ExtraData[1] != 0xad {
		t.Errorf("ExtraData = %x, want deadX2", g.ExtraData)
	}
	if (*big.Int)(g.Difficulty).Sign() != 0 {
		t.Errorf("Difficulty = %v, want 0 (post-Merge)", g.Difficulty)
	}
	if g.BaseFee == nil || (*big.Int)(g.BaseFee).Sign() == 0 {
		t.Error("BaseFee should be set (London active under prague fork)")
	}
	if len(g.Alloc) != 0 {
		t.Errorf("Alloc should be empty, got %d entries", len(g.Alloc))
	}
}

func TestBuildSynthetic_DefaultsApplied(t *testing.T) {
	g, err := BuildSynthetic("", nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic with zero values: %v", err)
	}
	if g.Config.ChainID.Int64() != 1337 {
		t.Errorf("default chainID = %d, want 1337", g.Config.ChainID.Int64())
	}
	if uint64(g.GasLimit) != 30_000_000 {
		t.Errorf("default GasLimit = %d, want 30_000_000", g.GasLimit)
	}
	if g.ExtraData == nil {
		t.Error("ExtraData should be empty slice not nil")
	}
	if !g.Config.IsPrague(big.NewInt(0), 0) {
		t.Error("default fork should be prague")
	}
}
