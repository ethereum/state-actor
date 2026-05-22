package specbuild

import (
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/internal/sizecal"
	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// BuildOptions carries the per-run inputs every Template.Expand needs.
type BuildOptions struct {
	// Seed drives deterministic name→address derivation and
	// synthesized storage generation.
	Seed int64
	// ClientName is "geth", "besu", "nethermind", or "reth".
	ClientName string
	// Sizer translates approximate_size_bytes → slot count.
	Sizer templates.SizeApproximator
	// TargetSize, when non-zero, caps the projected trie-DB footprint of the
	// returned PreAlloc. Walks the spec in declaration order, projecting each
	// entity's cost as
	//   sizecal.BytesPerAccount(client) +
	//   sizecal.BytesPerSlot(client) × Sizer.SlotsForBytes(client, e.ApproximateSizeBytes)
	// and drops all remaining entities once including the next would exceed
	// the cap. Deterministic across clients (same global constants + identical
	// walk order) — preserves the cross-client genesis-root invariance gate.
	TargetSize uint64
}

// Diagnostics carries non-fatal warnings surfaced to the CLI.
type Diagnostics struct {
	Warnings []string
}

// Build translates a parsed Spec into the flat slice of PreAllocEntity
// records each writer consumes. Per entity: resolve address → pick
// template → validate parameters → Expand. Collisions across emitted
// addresses (including synthesized) are rejected.
func Build(s *spec.Spec, opts BuildOptions) ([]templates.PreAllocEntity, Diagnostics, error) {
	var (
		out  []templates.PreAllocEntity
		diag Diagnostics
	)

	if s == nil || len(s.Entities) == 0 {
		return nil, diag, fmt.Errorf("Build: spec has no entities")
	}
	if opts.Sizer == nil {
		return nil, diag, fmt.Errorf("Build: BuildOptions.Sizer is required")
	}

	entities := truncateForTargetSize(s.Entities, opts, &diag)

	// Lower-case hex → first-emitting entity index.
	seenAddrs := make(map[string]int, len(entities))

	for i, e := range entities {
		tmpl, err := pickTemplate(e)
		if err != nil {
			return nil, diag, fmt.Errorf("entities[%d]: %w", i, err)
		}

		// Defense in depth for programmatic callers bypassing spec.Validate.
		if err := tmpl.ValidateParameters(e.Parameters); err != nil {
			return nil, diag, fmt.Errorf("entities[%d]: %w", i, err)
		}

		addr := ResolveAddress(opts.Seed, e, i)

		ctx := templates.Context{
			Seed:            opts.Seed,
			ClientName:      opts.ClientName,
			Sizer:           opts.Sizer,
			ResolvedAddress: addr,
			EntityIndex:     i,
		}

		expanded, err := tmpl.Expand(ctx, e)
		if err != nil {
			return nil, diag, fmt.Errorf("entities[%d] (%s.Expand): %w", i, tmpl.Name(), err)
		}

		for _, pe := range expanded {
			key := strings.ToLower(pe.Address.Hex())
			if prev, dup := seenAddrs[key]; dup {
				return nil, diag, fmt.Errorf(
					"entities[%d] (template %s) produced address %s that collides with entities[%d]",
					i, tmpl.Name(), pe.Address.Hex(), prev)
			}
			seenAddrs[key] = i
			out = append(out, pe)
		}
	}

	return out, diag, nil
}

// pickTemplate maps a spec entity to its handling template.
func pickTemplate(e spec.Entity) (templates.Template, error) {
	switch e.Kind {
	case spec.KindEOA:
		t, ok := templates.Lookup("eoa")
		if !ok {
			return nil, fmt.Errorf("registry missing required eoa template")
		}
		return t, nil
	case spec.KindContract:
		if e.Template != "" {
			t, ok := templates.Lookup(e.Template)
			if !ok {
				return nil, fmt.Errorf("template %q not registered", e.Template)
			}
			return t, nil
		}
		if len(e.Code) > 0 {
			t, ok := templates.Lookup("raw")
			if !ok {
				return nil, fmt.Errorf("registry missing required raw template")
			}
			return t, nil
		}
		return nil, fmt.Errorf("kind=contract requires either template: or code:")
	default:
		return nil, fmt.Errorf("unknown kind %q", e.Kind)
	}
}

var _ = (*common.Address)(nil)

// ProjectedCost returns the projected trie cost (in bytes) of the spec
// entity prefix that would survive truncation against opts.TargetSize.
// Internal/autofill calls this to compute the top-up budget as
// target_size − ProjectedCost without re-walking the entities.
//
// Uses the same per-entity cost formula as truncateForTargetSize so the
// two functions agree on which prefix is "kept". When opts.TargetSize is
// zero, no truncation applies and the result is the cost of every entity.
// A nil Sizer (or nil/empty Spec) yields 0, deferring the actual error to
// Build() which validates the inputs.
func ProjectedCost(s *spec.Spec, opts BuildOptions) uint64 {
	if s == nil || len(s.Entities) == 0 || opts.Sizer == nil {
		return 0
	}
	bAcct := sizecal.BytesPerAccount(opts.ClientName)
	bSlot := sizecal.BytesPerSlot(opts.ClientName)
	var running uint64
	for _, e := range s.Entities {
		var slotCost uint64
		if e.ApproximateSizeBytes > 0 {
			slotCost = uint64(opts.Sizer.SlotsForBytes(opts.ClientName, e.ApproximateSizeBytes)) * bSlot
		}
		cost := bAcct + slotCost
		if opts.TargetSize > 0 && running+cost > opts.TargetSize {
			return running
		}
		running += cost
	}
	return running
}

// truncateForTargetSize returns the longest prefix of entities whose
// projected trie cost fits inside opts.TargetSize. Returns the input
// unchanged when TargetSize is zero. Appends a Diagnostics warning when
// at least one entity is dropped so the CLI can surface the truncation.
//
// The per-entity cost is computed from the SPEC entity (pre-Expand) — we
// don't run Expand on entities we're about to discard. The cost formula
// uses the same Sizer the templates use to fan storage out, so the
// estimate tracks the actual slot count the writers will see.
func truncateForTargetSize(entities []spec.Entity, opts BuildOptions, diag *Diagnostics) []spec.Entity {
	if opts.TargetSize == 0 {
		return entities
	}
	bAcct := sizecal.BytesPerAccount(opts.ClientName)
	bSlot := sizecal.BytesPerSlot(opts.ClientName)
	var running uint64
	for i, e := range entities {
		var slotCost uint64
		if e.ApproximateSizeBytes > 0 {
			slotCost = uint64(opts.Sizer.SlotsForBytes(opts.ClientName, e.ApproximateSizeBytes)) * bSlot
		}
		cost := bAcct + slotCost
		if running+cost > opts.TargetSize {
			diag.Warnings = append(diag.Warnings, fmt.Sprintf(
				"--target-size %d B: truncated spec at entity[%d] (kept %d/%d); projected trie cost %d B would exceed budget",
				opts.TargetSize, i, i, len(entities), running+cost))
			return entities[:i]
		}
		running += cost
	}
	return entities
}
