package oracle

import (
	"testing"

	"github.com/ethereum/state-actor/internal/autofill"
)

// TestReproduce_Determinism — same cfg → identical (addr, code, slots)
// stream. Covers the load-bearing invariant: oracle tests rely on this
// to know what the writer wrote.
func TestReproduce_Determinism(t *testing.T) {
	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := ReproduceCfg{
		Seed:     12345,
		AutoFill: plan,
	}
	a1, c1 := Reproduce(cfg)
	a2, c2 := Reproduce(cfg)

	if len(a1) != plan.NumEOAs || len(a2) != plan.NumEOAs {
		t.Fatalf("EOA count: %d, %d, want %d", len(a1), len(a2), plan.NumEOAs)
	}
	if len(c1) != plan.NumContracts || len(c2) != plan.NumContracts {
		t.Fatalf("contract count: %d, %d, want %d", len(c1), len(c2), plan.NumContracts)
	}
	for i := range a1 {
		if a1[i].Address != a2[i].Address {
			t.Errorf("EOA[%d] address drift: %s vs %s", i, a1[i].Address.Hex(), a2[i].Address.Hex())
		}
	}
	for i := range c1 {
		if c1[i].Address != c2[i].Address {
			t.Errorf("contract[%d] address drift: %s vs %s", i, c1[i].Address.Hex(), c2[i].Address.Hex())
		}
		if string(c1[i].Code) != string(c2[i].Code) {
			t.Errorf("contract[%d] code drift", i)
		}
		if len(c1[i].Storage) != len(c2[i].Storage) {
			t.Errorf("contract[%d] slot count drift: %d vs %d", i, len(c1[i].Storage), len(c2[i].Storage))
		}
	}
}

// TestReproduce_DifferentSeeds — different seeds → different streams.
// Trivial check, but locks the contract that seeding actually matters.
func TestReproduce_DifferentSeeds(t *testing.T) {
	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	base := ReproduceCfg{
		Seed:     1,
		AutoFill: plan,
	}
	a1, _ := Reproduce(base)
	base.Seed = 2
	a2, _ := Reproduce(base)
	allSame := true
	for i := range a1 {
		if a1[i].Address != a2[i].Address {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("seed=1 and seed=2 produced identical EOA addresses; RNG not actually seeded")
	}
}
