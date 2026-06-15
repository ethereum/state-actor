package autofill

import (
	"fmt"
	mrand "math/rand"

	"github.com/ethereum/state-actor/internal/entitygen"
	"github.com/ethereum/state-actor/internal/sizecal"
)

// Plan is the deterministic recipe for filling target_size − spec_cost
// bytes of synthetic state in mainnet-shaped proportions. PlanForBudget
// constructs it once from the top-up budget; DrawEOA / DrawContract are
// then called per-entity by the writer until NumEOAs / NumContracts are
// exhausted (or the on-disk SizeTracker stops the loop early).
type Plan struct {
	NumEOAs        int
	NumContracts   int
	EOAFlavors     EOAFlavors
	StorageSampler Sampler
	CodeSampler    Sampler
}

// PlanForBudget builds a Plan that, if fully consumed, would emit roughly
// topUp bytes of synthetic state split 20 / 10 / 70 across account-trie /
// bytecode / storage (see internal/sizecal for the ratio constants).
//
// The contract count is pinned by the bytecode budget so per-contract code
// stays mainnet-realistic (truncated normal in [1 KiB, 24 KiB] centered at
// 5 KiB). Mean storage-per-contract is then derived from the storage
// budget so the math closes; with the canonical ratios it lands at ≈ 35 KiB
// regardless of topUp scale.
//
// Returns an error when topUp is below the account-trie cost of a single
// EOA (175 B). A non-nil Plan is safe to consume even when NumContracts is
// zero (e.g. when bytecode budget is below the minimum code size).
func PlanForBudget(topUp uint64) (*Plan, error) {
	bytesPerAccount := sizecal.BytesPerAccount("")
	if topUp < bytesPerAccount {
		return nil, fmt.Errorf("autofill: target_size %d B below one account (%d B)", topUp, bytesPerAccount)
	}

	budgetF := float64(topUp)
	bAcct := uint64(budgetF * sizecal.RatioAccount)
	bCode := uint64(budgetF * sizecal.RatioCode)
	bStorage := uint64(budgetF * sizecal.RatioStorage)

	var numContracts int
	storageSampler := Sampler{
		Min: sizecal.MinContractStorage,
		Max: sizecal.MaxContractStorage,
	}

	bytesPerSlot := sizecal.BytesPerSlot("")
	if bCode >= sizecal.MinContractCode && bStorage >= bytesPerSlot {
		// Round bCode / MeanContractCode → contract count.
		numContracts = int((bCode + sizecal.MeanContractCode/2) / sizecal.MeanContractCode)
		if numContracts > 0 {
			mean := float64(bStorage) / float64(numContracts)
			mean = max(mean, float64(sizecal.MinContractStorage))
			mean = min(mean, float64(sizecal.MaxContractStorage))
			storageSampler.Mean = mean
			storageSampler.Stddev = mean / 3
		}
	}

	accountUsed := uint64(numContracts) * bytesPerAccount
	var numEOAs int
	if bAcct > accountUsed {
		numEOAs = int((bAcct - accountUsed) / bytesPerAccount)
	}

	if numEOAs == 0 && numContracts == 0 {
		return nil, fmt.Errorf("autofill: budget %d B too small for any entity", topUp)
	}

	codeMean := float64(sizecal.MeanContractCode)
	return &Plan{
		NumEOAs:      numEOAs,
		NumContracts: numContracts,
		EOAFlavors:   DefaultEOAFlavors(),
		CodeSampler: Sampler{
			Mean:   codeMean,
			Stddev: codeMean / 3,
			Min:    sizecal.MinContractCode,
			Max:    sizecal.MaxContractCode,
		},
		StorageSampler: storageSampler,
	}, nil
}

// DrawEOA produces one synthetic EOA, advancing rng by the canonical 3
// entitygen.GenerateEOA draws plus 2 Bernoulli Float64s plus a conditional
// 20-byte read (when the delegation Bernoulli fires).
func (p *Plan) DrawEOA(rng *mrand.Rand) *entitygen.Account {
	return GenerateEOAFlavored(rng, p.EOAFlavors)
}

// DrawContract produces one synthetic contract with sampled code and
// storage sizes, then forwards to entitygen.GenerateContract. RNG draw
// order is fixed across all client emission sites:
//   1. CodeSampler.Draw(rng)    — code size in bytes (truncated normal).
//   2. StorageSampler.Draw(rng) — storage size in bytes; slot count =
//      bytes / sizecal.BytesPerSlot.
//   3. entitygen.GenerateContract(rng, codeSize*2/3, numSlots) — canonical
//      contract draw sequence. The 2/3 factor compensates for the
//      `codeSize + rng.Intn(codeSize)` doubling inside GenerateContract so
//      the realized mean lands at MeanContractCode.
func (p *Plan) DrawContract(rng *mrand.Rand) *entitygen.Account {
	codeBytes := p.CodeSampler.Draw(rng)
	codeSize := max(int(codeBytes*2/3), 1)

	storageBytes := p.StorageSampler.Draw(rng)
	numSlots := max(int(storageBytes/sizecal.BytesPerSlot("")), 1)

	return entitygen.GenerateContract(rng, codeSize, numSlots)
}
