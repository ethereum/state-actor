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

	warnTargetSizeBlindEntities(s.Entities, opts, &diag)
	entities := truncateForTargetSize(s.Entities, opts, &diag)

	if err := enforceArachnidFactoryRequirement(entities, opts); err != nil {
		return nil, diag, err
	}

	appendPatternResidentCodeWarnings(entities, &diag)

	// Lower-case hex → first-emitting entity index.
	seenAddrs := make(map[string]int, len(entities))

	for i, e := range entities {
		tmpl, err := pickTemplate(e)
		if err != nil {
			return nil, diag, fmt.Errorf("entities[%d]: %w", i, err)
		}

		// Reject entity-level fields (balance/nonce/code/
		// approximate_size_bytes) the template declares it ignores —
		// user-declared state must never silently disappear from the
		// generated prestate.
		if err := templates.CheckEntityFields(tmpl, e); err != nil {
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
		t, ok := templates.Lookup(templates.TemplateNameEOA)
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
			t, ok := templates.Lookup(templates.TemplateNameRaw)
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
// reads e.ApproximateSizeBytes. The six repricing templates introduced
// in PR 76 use template-specific sizing parameters (storage_pattern.final,
// create_preimage_deploys.count, create2_deploys.salt_count,
// sequential_eoas.count, sequential_pkey_eoas.count — plus the
// pre-existing erc20.total_owners path) that this projection does
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

// patternResidentWarnBytes: above this estimated unique-code residency
// we warn. Unique per-address pattern code is fully materialized by
// Expand and retained in generator.Config.GenesisCode for the whole run
// (only Storage streams), measured at ≈24.6 KB resident per derived
// contract. A production-minimum repricing fixture (150 000 × 24 576 B
// ≈ 3.4 GiB) warns BY DESIGN: that is the heads-up a laptop user needs
// and costs the bench host one log line.
const patternResidentWarnBytes uint64 = 2 << 30 // 2 GiB

// appendPatternResidentCodeWarnings warns per entity whose code_pattern
// fan-out is estimated to hold more than patternResidentWarnBytes of
// byte-unique runtime in memory. Shared-runtime (literal `runtime:`)
// entities alias one backing array and are exempt. Hard failure above
// templates' 64 GiB cap happens in parameter validation; this is the
// softer advisory tier. True code streaming is a tracked follow-up.
func appendPatternResidentCodeWarnings(entities []spec.Entity, diag *Diagnostics) {
	for i, e := range entities {
		var knob string
		switch e.Template {
		case templates.TemplateNameCreate2Deploys:
			knob = "salt_count"
		case templates.TemplateNameCreatePreimageDeploys:
			knob = "count"
		default:
			continue
		}
		pat, ok := e.Parameters["code_pattern"].(string)
		if !ok || templates.CodePatternRuntimeSize(pat) == 0 {
			continue
		}
		n, err := templates.ParseUint64Param(e.Parameters[knob], knob)
		if err != nil {
			continue // schema validation reports this with a better message
		}
		if est := n * templates.CodePatternRuntimeSize(pat); est > patternResidentWarnBytes {
			diag.Warnings = append(diag.Warnings, fmt.Sprintf(
				"entities[%d] (template %s): code_pattern %q materializes %d unique %d-byte runtimes ≈ %.1f GiB held in memory for the entire run (per-address code is not streamed, unlike storage); ensure the build host has the RAM headroom or lower %s (true code streaming is a tracked follow-up)",
				i, e.Template, pat, n, templates.CodePatternRuntimeSize(pat),
				float64(est)/float64(1<<30), knob))
		}
	}
}

// warnTargetSizeBlindEntities appends ONE diagnostics warning when
// opts.TargetSize > 0 and the spec contains entities whose on-disk
// footprint is derived from template parameters the size projection
// cannot see: the template ignores approximate_size_bytes AND takes
// parameters. (Parameterless templates such as create2_factory emit a
// single fixed-size account, which the projection prices correctly.
// erc20 with explicit total_owners is a residual gap — see
// TODO(template-aware-budget) on ProjectedCost.)
//
// Runs against the PRE-truncation entity list because
// truncateForTargetSize is exactly the walk that fails open here.
func warnTargetSizeBlindEntities(entities []spec.Entity, opts BuildOptions, diag *Diagnostics) {
	if opts.TargetSize == 0 {
		return
	}
	var blind []string
	for i, e := range entities {
		tmpl, err := pickTemplate(e)
		if err != nil {
			continue // the per-entity loop surfaces this as a hard error
		}
		if tmpl.HonoredEntityFields().ApproximateSizeBytes.Honored || len(e.Parameters) == 0 {
			continue
		}
		blind = append(blind, fmt.Sprintf("entities[%d] (template %s)", i, tmpl.Name()))
	}
	if len(blind) == 0 {
		return
	}
	diag.Warnings = append(diag.Warnings, fmt.Sprintf(
		"--target-size %d B: the following entities derive their size from template parameters the size projection cannot see: %s; --target-size can neither budget nor truncate their output, and the auto-fill top-up may overshoot the target",
		opts.TargetSize, strings.Join(blind, ", ")))
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
		if e.Template != templates.TemplateNameCreate2Deploys {
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
	// Arachnid address? Uses the template's own defaulting rule
	// (templates.EffectiveCreate2FactoryAddress) so this pass can never
	// drift from create2_factory.Expand.
	for i, e := range entities {
		if e.Template != templates.TemplateNameCreate2Factory {
			continue
		}
		if templates.EffectiveCreate2FactoryAddress(e, ResolveAddress(opts.Seed, e, i)) == arachnid {
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
