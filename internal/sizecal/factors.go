package sizecal

// SizeApproximator translates a target on-disk byte budget into a synthetic
// storage-slot count. The client parameter is reserved for per-client overrides;
// the default implementation ignores it.
type SizeApproximator interface {
	SlotsForBytes(client string, targetBytes uint64) int
}

// bytesPerSlot is the TRIE-only on-disk B/slot cost. Flat-state rows (Pebble
// snapshot, Bonsai flat, reth MDBX flat tables, Nethermind flat Account/Storage
// rows) are ADDITIONAL and not counted toward target_bytes. Calibration anchor
// and per-client flat-state overhead numbers live in docs/CALIBRATION.md.
const bytesPerSlot uint64 = 140

// bytesPerAccount is the TRIE-only on-disk B/account cost.
const bytesPerAccount uint64 = 175

// Mainnet-shaped distribution ratios applied by internal/autofill to the
// top-up portion of the target budget (target_size minus the projected spec
// cost). They do NOT describe the final DB total — a spec biased toward
// storage will leave the realized DB skewed even after auto-fill runs.
const (
	RatioAccount = 0.20
	RatioCode    = 0.10
	RatioStorage = 0.70
)

// Per-contract code-size bounds for synthetic auto-fill contracts.
// internal/autofill samples a truncated normal N(MeanContractCode,
// ≈MeanContractCode/3) clamped to [MinContractCode, MaxContractCode].
const (
	MinContractCode  uint64 = 1 << 10
	MaxContractCode  uint64 = 24 << 10
	MeanContractCode uint64 = 5 << 10
)

// Per-contract storage-size bounds for synthetic auto-fill contracts.
// internal/autofill samples a truncated normal whose mean is budget-derived
// then clamped to [MinContractStorage, MaxContractStorage].
const (
	MinContractStorage uint64 = 1 << 10
	MaxContractStorage uint64 = 100 << 20
)

// Default returns the package-level SizeApproximator backed by the single
// global trie-only constant. Identical across clients — required by the
// cross-client genesis-root invariance gate.
func Default() SizeApproximator {
	return defaultSizer{}
}

// NewFixed returns a SizeApproximator that always uses the given bytes-per-slot
// ratio. Used by tests to decouple test sizing from the production constant.
func NewFixed(bytesPerSlot uint64) SizeApproximator {
	return fixedSizer{bytesPerSlot: bytesPerSlot}
}

// BytesPerSlot returns the global trie-only on-disk B/slot cost.
func BytesPerSlot(_ string) uint64 {
	return bytesPerSlot
}

// BytesPerAccount returns the global trie-only on-disk B/account cost.
func BytesPerAccount(_ string) uint64 {
	return bytesPerAccount
}

type defaultSizer struct{}

func (defaultSizer) SlotsForBytes(_ string, targetBytes uint64) int {
	if bytesPerSlot == 0 {
		return 0
	}
	return int(targetBytes / bytesPerSlot)
}

type fixedSizer struct{ bytesPerSlot uint64 }

func (s fixedSizer) SlotsForBytes(_ string, targetBytes uint64) int {
	if s.bytesPerSlot == 0 {
		return 0
	}
	return int(targetBytes / s.bytesPerSlot)
}
