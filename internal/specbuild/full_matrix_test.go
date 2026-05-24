package specbuild

import (
	"testing"

	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestBuildFullMatrix parses + validates + builds
// examples/full-matrix-spec-feature.yaml against every supported client
// calibration. Pins the entity count + cross-client PreAlloc count
// equality so a future change to either the YAML or the templates
// surfaces here before it reaches CI's e2e jobs.
//
// The full-matrix YAML is the canonical fixture every per-client e2e
// suite loads (via internal/e2e_testing.LoadCISpec): same YAML, same
// seed, same sizer → byte-identical PreAlloc across all four MPT
// clients. This test enforces COUNT equality only; byte-identity is
// enforced downstream by the cross-client-genesis-root aggregator
// job in .github/workflows/ci.yml. Count divergence here therefore
// indicates a template/parser drift; byte divergence detected by the
// aggregator indicates a codec/calibration drift.
func TestBuildFullMatrix(t *testing.T) {
	s, err := spec.ParseFile("../../examples/full-matrix-spec-feature.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, want := len(s.Entities), 22; got != want {
		t.Fatalf("entity count: got %d, want %d", got, want)
	}

	// Build under each client calibration; the result count must be
	// identical (cross-client invariant) and ≥ entity count (templates
	// fan out to multiple PreAllocEntity records).
	var firstClientPreAllocLen int
	for _, client := range []string{"geth", "besu", "nethermind", "reth"} {
		opts := BuildOptions{
			Seed:       42,
			ClientName: client,
			Sizer:      fixedSizer{bytesPerSlot: 64},
		}
		pre, diag, err := Build(s, opts)
		if err != nil {
			t.Fatalf("Build %s: %v", client, err)
		}
		if len(diag.Warnings) != 0 {
			t.Errorf("Build %s: unexpected warnings: %v", client, diag.Warnings)
		}
		if len(pre) < len(s.Entities) {
			t.Errorf("Build %s: PreAlloc count %d < entity count %d", client, len(pre), len(s.Entities))
		}
		if firstClientPreAllocLen == 0 {
			firstClientPreAllocLen = len(pre)
			continue
		}
		if got := len(pre); got != firstClientPreAllocLen {
			t.Errorf("Build %s: PreAlloc count %d differs from first client's %d — cross-client invariant broken",
				client, got, firstClientPreAllocLen)
		}
	}
}
