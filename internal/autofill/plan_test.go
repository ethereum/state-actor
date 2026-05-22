package autofill

import (
	"testing"

	"github.com/nerolation/state-actor/internal/sizecal"
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
