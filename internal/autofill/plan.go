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

// Profile selects the account/code/storage byte-split AutoFill targets.
type Profile int

const (
	// ProfileMainnet is the default 20/10/70 account/code/storage split.
	ProfileMainnet Profile = iota
	// ProfileAccounts biases the entire budget to EOAs (account-trie only:
	// no contracts, no storage). It generates account-dominated tries that
	// stress the account-domain index + commitment paths at scale while
	// staying on the bounded streaming DrawEOA path — e.g. the 100 GB
	// account-heavy low-memory bench. DrawEOA's RNG draw sequence is
	// unchanged, so cross-client state-root invariance holds as long as
	// every client uses the same profile + seed.
	ProfileAccounts
)

// ratios returns the (account, code, storage) byte-budget fractions for p.
func (p Profile) ratios() (account, code, storage float64) {
	if p == ProfileAccounts {
		return 1.0, 0.0, 0.0
	}
	return sizecal.RatioAccount, sizecal.RatioCode, sizecal.RatioStorage
}

// ParseProfile maps a CLI string ("" / "mainnet" / "accounts") to a Profile.
func ParseProfile(s string) (Profile, error) {
	switch s {
	case "", "mainnet":
		return ProfileMainnet, nil
	case "accounts":
		return ProfileAccounts, nil
	default:
		return ProfileMainnet, fmt.Errorf("autofill: unknown profile %q (want 'mainnet' or 'accounts')", s)
	}
}

// PlanForBudget builds a mainnet-shaped (20/10/70) Plan. Thin wrapper over
// PlanForBudgetProfile(topUp, ProfileMainnet).
func PlanForBudget(topUp uint64) (*Plan, error) {
	return PlanForBudgetProfile(topUp, ProfileMainnet)
}

// PlanForBudgetProfile builds a Plan that, if fully consumed, would emit
// roughly topUp bytes of synthetic state split per the profile's
// account/code/storage ratios (mainnet = 20/10/70; accounts = 100/0/0).
//
// The contract count is pinned by the bytecode budget so per-contract code
// stays mainnet-realistic (truncated normal in [1 KiB, 24 KiB] centered at
// 5 KiB). Mean storage-per-contract is then derived from the storage
// budget so the math closes; with the canonical ratios it lands at ≈ 35 KiB
// regardless of topUp scale. ProfileAccounts zeroes the code+storage
// budgets, so numContracts == 0 and the whole budget becomes EOAs.
//
// Returns an error when topUp is below the account-trie cost of a single
// EOA (175 B). A non-nil Plan is safe to consume even when NumContracts is
// zero (e.g. when bytecode budget is below the minimum code size).
func PlanForBudgetProfile(topUp uint64, profile Profile) (*Plan, error) {
	bytesPerAccount := sizecal.BytesPerAccount("")
	if topUp < bytesPerAccount {
		return nil, fmt.Errorf("autofill: target_size %d B below one account (%d B)", topUp, bytesPerAccount)
	}

	rAccount, rCode, rStorage := profile.ratios()
	budgetF := float64(topUp)
	bAcct := uint64(budgetF * rAccount)
	bCode := uint64(budgetF * rCode)
	bStorage := uint64(budgetF * rStorage)

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
