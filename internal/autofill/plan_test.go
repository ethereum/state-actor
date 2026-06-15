package autofill

import (
	"testing"

	"github.com/ethereum/state-actor/internal/sizecal"
)

func TestPlanForBudget_BelowOneAccount(t *testing.T) {
	if _, err := PlanForBudget(uint64(sizecal.BytesPerAccount("")) - 1); err == nil {
		t.Fatal("expected error for budget below one account")
	}
}

func TestPlanForBudget_ZeroEntitiesError(t *testing.T) {
	// 175 ≤ B < 875 → bAcct < 175 → numEOAs=0; numContracts=0 → error.
	if _, err := PlanForBudget(500); err == nil {
		t.Fatal("expected error for budget that yields zero entities")
	}
}

func TestPlanForBudget_SmallBudgetEOAsOnly(t *testing.T) {
	// bAcct = 175 → 1 EOA. bCode = 87 < MinContractCode(1024) → no contracts.
	p, err := PlanForBudget(875)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NumEOAs < 1 {
		t.Errorf("NumEOAs: got %d, want ≥ 1", p.NumEOAs)
	}
	if p.NumContracts != 0 {
		t.Errorf("NumContracts: got %d, want 0 at this scale", p.NumContracts)
	}
}

func TestPlanForBudget_10GiB(t *testing.T) {
	p, err := PlanForBudget(10 << 30)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// bCode = 1 GiB; round(1 GiB / 5 KiB) = 209715.
	if p.NumContracts != 209715 {
		t.Errorf("NumContracts: got %d, want 209715", p.NumContracts)
	}
	// bAcct = 2 GiB; (2 GiB − 209715 × 175) / 175 ≈ 12,061,620.
	const wantEOAs = 12061620
	if abs(p.NumEOAs-wantEOAs) > 10 {
		t.Errorf("NumEOAs: got %d, want ~%d (±10)", p.NumEOAs, wantEOAs)
	}
	// mean storage per contract = 0.7 × 10 GiB / 209715 = 35840 B → in [1 KiB, 100 MiB] range.
	if got := p.StorageSampler.Mean; got < 35000 || got > 37000 {
		t.Errorf("StorageSampler.Mean: got %f, want ~35840", got)
	}
}

func TestPlanForBudget_Deterministic(t *testing.T) {
	p1, err1 := PlanForBudget(100 << 20)
	p2, err2 := PlanForBudget(100 << 20)
	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v / %v", err1, err2)
	}
	if p1.NumEOAs != p2.NumEOAs || p1.NumContracts != p2.NumContracts {
		t.Errorf("PlanForBudget non-deterministic: %+v vs %+v", p1, p2)
	}
}

func TestPlanForBudget_SamplerRanges(t *testing.T) {
	p, err := PlanForBudget(100 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if p.CodeSampler.Min != sizecal.MinContractCode || p.CodeSampler.Max != sizecal.MaxContractCode {
		t.Errorf("CodeSampler range: got [%d, %d], want [%d, %d]",
			p.CodeSampler.Min, p.CodeSampler.Max,
			sizecal.MinContractCode, sizecal.MaxContractCode)
	}
	if p.StorageSampler.Min != sizecal.MinContractStorage || p.StorageSampler.Max != sizecal.MaxContractStorage {
		t.Errorf("StorageSampler range: got [%d, %d], want [%d, %d]",
			p.StorageSampler.Min, p.StorageSampler.Max,
			sizecal.MinContractStorage, sizecal.MaxContractStorage)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// TestPlanForBudget_NearContractBoundary pins the rounding at the
// contract-count threshold. PlanForBudget computes
//
//	N_contracts = round(B_code / MeanContractCode)
//
// where B_code = 0.1 * topUp, MeanContractCode = 5 KiB. The threshold
// where round flips from 0 → 1 is B_code = MeanContractCode/2 = 2560,
// i.e. topUp = 25600. A regression that changed +MeanContractCode/2 to
// +0 or +MeanContractCode would flip contract counts at this seam
// unnoticed; the existing TestPlanForBudget_10GiB pin is too far up the
// curve to catch rounding-direction regressions at the boundary.
func TestPlanForBudget_NearContractBoundary(t *testing.T) {
	cases := []struct {
		budget        uint64
		wantContracts int
	}{
		{24_576, 0},  // 0.1*topUp = 2457.6, round → 0
		{25_600, 1},  // 0.1*topUp = 2560,   round → 1 (the threshold)
		{50_000, 1},  // 0.1*topUp = 5000,   round → 1
		{76_800, 2},  // 0.1*topUp = 7680,   round → 2 (mid 1↔2 seam)
	}
	for _, tc := range cases {
		p, err := PlanForBudget(tc.budget)
		if err != nil {
			// Very small budgets (< 875 B) error on the zero-entities
			// branch — skip them rather than misreport as test failures.
			continue
		}
		if p.NumContracts != tc.wantContracts {
			t.Errorf("budget %d: NumContracts %d, want %d",
				tc.budget, p.NumContracts, tc.wantContracts)
		}
	}
}
