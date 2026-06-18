package templates

import "github.com/nerolation/state-actor/internal/spec"

// fixedSizer is a SizeApproximator stub for tests. The real per-client
// calibration lives in internal/sizecal/; templates should never depend on
// that package, so tests use this fixed-ratio impl.
type fixedSizer struct {
	bytesPerSlot uint64
}

func (s fixedSizer) SlotsForBytes(client string, targetBytes uint64) int {
	if s.bytesPerSlot == 0 {
		return 0
	}
	return int(targetBytes / s.bytesPerSlot)
}

// mkContractEntity builds a spec.Entity for kind=contract with the given
// template + parameters. Used by template tests so the test file doesn't
// have to import spec just for one struct literal.
func mkContractEntity(template string, params map[string]any) spec.Entity {
	return spec.Entity{
		Kind:       spec.KindContract,
		Template:   template,
		Parameters: params,
	}
}
