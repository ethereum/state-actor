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

	if err := enforceArachnidFactoryRequirement(entities, opts); err != nil {
		return nil, diag, err
	}

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
//
// TODO(template-aware-budget): the per-entity cost formula below only
// reads e.ApproximateSizeBytes. The five repricing templates introduced
// in PR 76 use template-specific sizing parameters (storage_pattern.final,
// create_preimage_deploys.count, create2_deploys.salt_count,
// sequential_eoas.count, erc20.total_owners) that this projection does
// not see — so an entity that emits millions of slots/accounts is
// budgeted at one bAcct (~175 B). The downstream effects are:
//   (1) autofill's headroom = target_size - ProjectedCost overshoots:
//       the auto-fill adds ~target_size's worth of synthetic state on
//       top of spec storage that the projection missed (observed in
//       the 10 GB rehearsal: spec ~12 GB + autofill 20 GB → 38 GB on
//       disk, vs intended ~20 GB).
//   (2) truncateForTargetSize fails open: a spec writing 100 GB of
//       storage_pattern entities with --target-size=50GB silently
//       produces 100+ GB instead of truncating.
// Fix shape (separate PR): extend the Template interface with a
// ProjectCost(opts, entity) (uint64, error) method that each template
// implements from its own parameter schema. Then ProjectedCost +
// truncateForTargetSize call tmpl.ProjectCost instead of using this
// formula. Cross-checked at unit level by a regression asserting
// ProjectCost == bytes(Expand(...).slots, accounts) for each template.
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

// enforceArachnidFactoryRequirement implements the cross-entity invariant
// for `create2_deploys` ↔ `create2_factory` pairing:
//
//	If any `create2_deploys` entity uses the canonical Arachnid factory
//	(i.e. its `factory:` parameter is unset or explicitly set to
//	templates.CanonicalCREATE2FactoryAddress), then at least one
//	`create2_factory` entity MUST resolve to that same address.
//
// The check is intentionally narrow — only the Arachnid case is
// enforced. Users who deploy via a custom factory at a non-Arachnid
// address take responsibility for declaring (or not declaring) a
// matching `create2_factory` entity themselves; specbuild won't block
// them.
//
// Caught at parse time so the user fixes the spec before kicking off a
// multi-hour generation that would otherwise produce a chaindata whose
// CREATE2-derived contracts call into an empty 0x4e59…956c.
func enforceArachnidFactoryRequirement(entities []spec.Entity, opts BuildOptions) error {
	arachnid := templates.CanonicalCREATE2FactoryAddress

	// First pass: does any create2_deploys entry reference the Arachnid
	// factory (either by default — factory: omitted — or explicitly)?
	consumerIdx := -1
	for i, e := range entities {
		if e.Template != "create2_deploys" {
			continue
		}
		factory := arachnid
		if v, has := e.Parameters["factory"]; has {
			parsed, err := templates.ParseAddressParam(v, "factory")
			if err != nil {
				// Skip — schema validation in tmpl.ValidateParameters will
				// surface this with a clearer error than we could here.
				continue
			}
			factory = parsed
		}
		if factory == arachnid {
			consumerIdx = i
			break
		}
	}
	if consumerIdx == -1 {
		return nil
	}

	// Second pass: is there a create2_factory entity that resolves to the
	// Arachnid address? Mirrors create2_factory.Expand's defaulting:
	// `address:` unset AND `name:` unset → Arachnid; otherwise honor the
	// resolved address.
	for i, e := range entities {
		if e.Template != "create2_factory" {
			continue
		}
		var addr common.Address
		if e.Address == nil && e.Name == "" {
			addr = arachnid
		} else {
			addr = ResolveAddress(opts.Seed, e, i)
		}
		if addr == arachnid {
			return nil
		}
	}

	return fmt.Errorf(
		"entities[%d] (template create2_deploys) deploys via the canonical Arachnid factory at %s but no create2_factory entity resolves to that address; add\n"+
			"  - kind: contract\n"+
			"    template: create2_factory\n"+
			"to the spec (the template defaults to %s when neither `address:` nor `name:` is set)",
		consumerIdx, arachnid.Hex(), arachnid.Hex())
}
