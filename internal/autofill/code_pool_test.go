package autofill

import (
	mrand "math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/internal/sizecal"
)

// Contracts share code from a pool of DistinctBytecodes entries, each sized
// within the CodeSampler bounds.
func TestDrawContract_SharesPoolCode(t *testing.T) {
	p, err := PlanForBudget(10 << 20) // ~205 contracts → 7 distinct codes
	if err != nil {
		t.Fatal(err)
	}
	rng := mrand.New(mrand.NewSource(1))
	seen := map[common.Hash]bool{}
	for i := range p.NumContracts {
		c := p.DrawContract(rng)
		seen[c.CodeHash] = true
		if n := uint64(len(c.Code)); n < sizecal.MinContractCode || n > sizecal.MaxContractCode {
			t.Fatalf("contract %d: %d B code outside [%d, %d]", i, n, sizecal.MinContractCode, sizecal.MaxContractCode)
		}
	}
	if len(seen) != p.DistinctBytecodes {
		t.Fatalf("distinct codes %d, want %d", len(seen), p.DistinctBytecodes)
	}
}
