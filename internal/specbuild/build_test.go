package specbuild

import (
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/sizecal"
	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// fixedSizer is a SizeApproximator stub for tests that drive
// approximate_size_bytes expansion deterministically. The truncation
// tests (TestBuild_TargetSize*) read sizecal.BytesPerAccount directly
// because that constant lives at the production specbuild↔sizecal seam.
type fixedSizer struct{ bytesPerSlot uint64 }

func (s fixedSizer) SlotsForBytes(client string, bytes uint64) int {
	if s.bytesPerSlot == 0 {
		return 0
	}
	return int(bytes / s.bytesPerSlot)
}

var defaultOpts = BuildOptions{
	Seed:       42,
	ClientName: "geth",
	Sizer:      fixedSizer{bytesPerSlot: 64},
}

func parseSpec(t *testing.T, src string) *spec.Spec {
	t.Helper()
	s, err := spec.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

// TestPatternResidentCodeWarnings pins the I5 advisory tier: above
// 2 GiB estimated unique-runtime residency a per-entity diagnostics
// warning fires. Calls the helper directly (no Build, no Expand) so the
// boundary counts stay instant. Measured basis: ≈24.6 KB resident per
// pattern contract; a 150k-contract production fixture (≈3.4 GiB) warns
// BY DESIGN, while small fixtures (e.g. full-matrix's salt_count=2) stay
// silent (see TestBuildFullMatrix's warning-free assertion).
func TestPatternResidentCodeWarnings(t *testing.T) {
	mk := func(template string, params map[string]any) spec.Entity {
		return spec.Entity{Kind: spec.KindContract, Template: template, Parameters: params}
	}
	cases := []struct {
		name     string
		entity   spec.Entity
		wantWarn bool
		contains string
	}{
		{"just under 2 GiB", mk("create2_deploys", map[string]any{
			"code_pattern": "unique_jumpdest_pre_amsterdam", "salt_count": 87381,
		}), false, ""},
		{"just over 2 GiB", mk("create2_deploys", map[string]any{
			"code_pattern": "unique_jumpdest_pre_amsterdam", "salt_count": 87382,
		}), true, "2.0 GiB"},
		{"min-fixture scale", mk("create2_deploys", map[string]any{
			"code_pattern": "unique_jumpdest_pre_amsterdam", "salt_count": 150000,
		}), true, "3.4 GiB"},
		{"preimage pattern over threshold", mk("create_preimage_deploys", map[string]any{
			"code_pattern": "unique_jumpdest_pre_amsterdam", "sender": "0x000000000000000000000000000000000000beef", "count": 150000,
		}), true, "3.4 GiB"},
		{"shared runtime is exempt", mk("create_preimage_deploys", map[string]any{
			"runtime": "0x00", "sender": "0x000000000000000000000000000000000000beef", "count": 1000000,
		}), false, ""},
		{"garbage count deferred to schema validation", mk("create2_deploys", map[string]any{
			"code_pattern": "unique_jumpdest_pre_amsterdam", "salt_count": "lots",
		}), false, ""},
	}
	for _, c := range cases {
		var diag Diagnostics
		appendPatternResidentCodeWarnings([]spec.Entity{c.entity}, &diag)
		if got := len(diag.Warnings) > 0; got != c.wantWarn {
			t.Errorf("%s: warned=%v, want %v (%v)", c.name, got, c.wantWarn, diag.Warnings)
			continue
		}
		if c.wantWarn && !strings.Contains(diag.Warnings[0], c.contains) {
			t.Errorf("%s: warning should contain %q; got: %s", c.name, c.contains, diag.Warnings[0])
		}
	}
}

// TestBuildWarnsTargetSizeBlindTemplates pins the I4 fix: --target-size
// budgets entities at ~bytesPerAccount via e.ApproximateSizeBytes only,
// so a storage_pattern entity expanding 1001 slots (~140 KB real trie
// cost) sailed under a 1000-byte TargetSize with ZERO warnings
// (demonstrated against the unfixed tree) — truncateForTargetSize fails
// open and the autofill top-up then overshoots. Build now emits exactly
// ONE diagnostics warning naming every cost-blind entity. (The real fix
// — a per-template ProjectCost — stays the TODO(template-aware-budget)
// follow-up; this is the user-facing tripwire.)
func TestBuildWarnsTargetSizeBlindTemplates(t *testing.T) {
	mkStoragePattern := func(addr string) spec.Entity {
		a := spec.HexAddress(common.HexToAddress(addr))
		return spec.Entity{
			Kind:       spec.KindContract,
			Template:   "storage_pattern",
			Address:    &a,
			Parameters: map[string]any{"final": 1000},
		}
	}
	countBlindWarnings := func(diag Diagnostics) int {
		n := 0
		for _, w := range diag.Warnings {
			if strings.Contains(w, "size projection cannot see") {
				n++
			}
		}
		return n
	}

	// TargetSize > 0 (well above the ~175 B projected cost, far below the
	// ~140 KB real cost) → exactly one warning naming the template.
	opts := defaultOpts
	opts.TargetSize = 1000
	s := &spec.Spec{Entities: []spec.Entity{mkStoragePattern("0x0000000000000000000000000000000000005000")}}
	pre, diag, err := Build(s, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pre) != 1 {
		t.Fatalf("entity count: got %d, want 1 (must build, not truncate)", len(pre))
	}
	if n := countBlindWarnings(diag); n != 1 {
		t.Fatalf("blind-template warnings: got %d, want 1 (warnings: %v)", n, diag.Warnings)
	}
	if !strings.Contains(diag.Warnings[0], "storage_pattern") {
		t.Errorf("warning must name the template; got: %s", diag.Warnings[0])
	}

	// TargetSize == 0 → no warning.
	_, diag, err = Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := countBlindWarnings(diag); n != 0 {
		t.Errorf("TargetSize=0: got %d blind warnings, want 0 (%v)", n, diag.Warnings)
	}

	// Two blind entities → still exactly ONE warning, listing both anchors.
	s2 := &spec.Spec{Entities: []spec.Entity{
		mkStoragePattern("0x0000000000000000000000000000000000005000"),
		mkStoragePattern("0x0000000000000000000000000000000000006000"),
	}}
	_, diag, err = Build(s2, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := countBlindWarnings(diag); n != 1 {
		t.Errorf("two blind entities: got %d warnings, want 1 (%v)", n, diag.Warnings)
	}
	if len(diag.Warnings) > 0 && (!strings.Contains(diag.Warnings[0], "entities[0]") || !strings.Contains(diag.Warnings[0], "entities[1]")) {
		t.Errorf("warning must list both anchors; got: %s", diag.Warnings[0])
	}

	// Parameterless template (create2_factory: one fixed account the
	// projection prices correctly) → no warning.
	s3 := &spec.Spec{Entities: []spec.Entity{{
		Kind:     spec.KindContract,
		Template: "create2_factory",
	}}}
	_, diag, err = Build(s3, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := countBlindWarnings(diag); n != 0 {
		t.Errorf("create2_factory-only: got %d blind warnings, want 0 (%v)", n, diag.Warnings)
	}
}

// TestBuildRejectsIgnoredEntityFields pins the C1 fix: entity-level
// `balance:` on a template that only reads parameters.balance used to be
// SILENTLY ignored — Build returned 1-wei accounts with zero warnings
// (demonstrated against the unfixed tree), so a 150k-sender pool the
// spec said was funded came out holding 1 wei each, discovered only when
// the benchmark's first value-bearing transaction failed. Build now
// rejects the entity outright, naming the parameters-level alternative.
func TestBuildRejectsIgnoredEntityFields(t *testing.T) {
	anchor := spec.HexAddress(common.HexToAddress("0x0000000000000000000000000000000000001000"))
	bal := spec.BigIntDecimal{V: uint256.NewInt(5_000_000_000_000_000_000)}
	s := &spec.Spec{Entities: []spec.Entity{{
		Kind:       spec.KindContract,
		Template:   "sequential_eoas",
		Address:    &anchor,
		Balance:    &bal,
		Parameters: map[string]any{"count": 3},
	}}}
	pre, diag, err := Build(s, defaultOpts)
	if err == nil {
		got := "no entities"
		if len(pre) > 0 {
			got = fmt.Sprintf("%d entities, first balance=%s", len(pre), pre[0].Account.Balance)
		}
		t.Fatalf("expected error for ignored entity-level balance; got nil (%s, %d warnings)", got, len(diag.Warnings))
	}
	for _, want := range []string{"entity-level", "parameters.balance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q; got: %v", want, err)
		}
	}
}

// TestBuildAllowsHonoredEntityFields is the control for the C1 fix:
// storage_pattern honors entity-level nonce/balance (the shipped
// repricing fixtures rely on exactly this shape), so it must keep
// building — and the values must actually land in the account.
func TestBuildAllowsHonoredEntityFields(t *testing.T) {
	anchor := spec.HexAddress(common.HexToAddress("0x0000000000000000000000000000000000004000"))
	bal := spec.BigIntDecimal{V: uint256.NewInt(7)}
	s := &spec.Spec{Entities: []spec.Entity{{
		Kind:       spec.KindContract,
		Template:   "storage_pattern",
		Address:    &anchor,
		Nonce:      1,
		Balance:    &bal,
		Parameters: map[string]any{"final": 5},
	}}}
	pre, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pre) != 1 || pre[0].Account.Nonce != 1 || pre[0].Account.Balance.Uint64() != 7 {
		t.Fatalf("storage_pattern entity-level fields not honored: %+v", pre[0].Account)
	}
}

func TestBuildStory1(t *testing.T) {
	// Story 1: three ERC-20s of decreasing size + five 7702 EOAs.
	s, err := spec.ParseFile("../../examples/spec-erc20-mixed-sizes.yaml")
	if err != nil {
		t.Fatalf("load story 1: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	pre, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := len(pre), 8; got != want {
		t.Fatalf("entity count: got %d, want %d", got, want)
	}
	// First three are ERC-20s — should carry non-empty Code (the runtime
	// bytecode) and a non-nil Storage iterator.
	for i := 0; i < 3; i++ {
		if len(pre[i].Code) == 0 {
			t.Errorf("entity[%d] (erc20): Code is empty", i)
		}
		if pre[i].Storage == nil {
			t.Errorf("entity[%d] (erc20): Storage is nil", i)
		}
	}
	// Last five are 7702 EOAs — Code is the 23-byte delegation marker.
	for i := 3; i < 8; i++ {
		if len(pre[i].Code) != 23 {
			t.Errorf("entity[%d] (eoa): Code length = %d, want 23", i, len(pre[i].Code))
		}
	}
}

func TestBuildStory2(t *testing.T) {
	// Story 2: three bloated 7702 EOAs.
	s, err := spec.ParseFile("../../examples/spec-eoa-bloat.yaml")
	if err != nil {
		t.Fatalf("load story 2: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	pre, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := len(pre), 3; got != want {
		t.Fatalf("entity count: got %d, want %d", got, want)
	}
	for i, e := range pre {
		if e.Storage == nil {
			t.Errorf("entity[%d] (bloated eoa): Storage is nil — bloat slots missing", i)
		}
	}
}

func TestBuildAllFeatures(t *testing.T) {
	// Mixed-spec fixture exercising every schema feature.
	s, err := spec.ParseFile("../spec/testdata/valid-all-features.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	pre, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pre) != 7 {
		t.Fatalf("entity count: got %d, want 7", len(pre))
	}

	// entities[0]: explicit address 0x...aaaa
	wantExplicit := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	if pre[0].Address != wantExplicit {
		t.Errorf("entity[0] (explicit) address: got %v, want %v", pre[0].Address, wantExplicit)
	}
	// entities[1]: name-derived (named-token). Address must be stable.
	addr1Run1 := pre[1].Address
	pre2, _, _ := Build(s, defaultOpts)
	if pre2[1].Address != addr1Run1 {
		t.Errorf("entity[1] (name-derived) must be stable across runs: %v vs %v",
			addr1Run1, pre2[1].Address)
	}
}

func TestBuildPositionDerivedReordering(t *testing.T) {
	// Two NAMED entities at positions 0 and 1. Swapping them: the
	// name-derived address follows the entity's `name:` field — NOT its
	// position — so entity "alpha" lands at the same address in both
	// specs. (Top-level `name:` is what derive.go uses; the `name:` field
	// inside `parameters:` is template-specific metadata, distinct.)
	yamlA := `entities:
  - kind: contract
    template: erc20
    name: alpha
    parameters:
      symbol: A
      name: A
      decimals: 18
  - kind: contract
    template: erc20
    name: beta
    parameters:
      symbol: B
      name: B
      decimals: 18`

	yamlB := `entities:
  - kind: contract
    template: erc20
    name: beta
    parameters:
      symbol: B
      name: B
      decimals: 18
  - kind: contract
    template: erc20
    name: alpha
    parameters:
      symbol: A
      name: A
      decimals: 18`

	specA := parseSpec(t, yamlA)
	specB := parseSpec(t, yamlB)

	// Both have name-derived addresses (name: "A" and name: "B"). Swap
	// position → addresses must follow the names, not the position,
	// because name-derivation wins over position-derivation. So entity A
	// is at the same address in both specs.
	preA, _, _ := Build(specA, defaultOpts)
	preB, _, _ := Build(specB, defaultOpts)
	// In specA, entity 0 = name "alpha". In specB, entity 1 = name "alpha".
	if preA[0].Address != preB[1].Address {
		t.Errorf("named entity 'alpha' should match across reorderings: %v vs %v",
			preA[0].Address, preB[1].Address)
	}
}

func TestBuildPositionDerivedDependsOnIndex(t *testing.T) {
	// Two TRULY anonymous entities (no name, no address). Reordering
	// changes their derived addresses.
	yamlA := `entities:
  - kind: contract
    code: "0x01"
  - kind: contract
    code: "0x02"`

	yamlB := `entities:
  - kind: contract
    code: "0x02"
  - kind: contract
    code: "0x01"`

	preA, _, _ := Build(parseSpec(t, yamlA), defaultOpts)
	preB, _, _ := Build(parseSpec(t, yamlB), defaultOpts)

	// In specA, entity 0 has code 0x01. In specB, entity 1 has code 0x01.
	// Their *addresses* are both position-derived from indices 0 vs 1, so
	// they should DIFFER even though the entity content is the same.
	if preA[0].Address == preB[1].Address {
		t.Errorf("position-derived addresses must depend on index, not content")
	}
}

func TestBuildDetectsCrossEntityAddressCollision(t *testing.T) {
	// Two entities with different names but engineered (here, just same
	// name) to produce the same derived address. spec.Validate doesn't
	// catch this because explicit-address dup check only covers explicit
	// addresses; the post-expansion check in Build does.
	yamlSrc := `entities:
  - kind: contract
    template: erc20
    name: collider
    parameters:
      symbol: A
      name: A
      decimals: 18
  - kind: contract
    template: erc20
    name: collider
    parameters:
      symbol: B
      name: B
      decimals: 18`

	s := parseSpec(t, yamlSrc)
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate should pass (same name is permitted at parse time): %v", err)
	}
	_, _, err := Build(s, defaultOpts)
	if err == nil {
		t.Fatal("Build should detect collision between same-named entities")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("expected collision error, got %q", err.Error())
	}
}

func TestBuildRejectsNilSizer(t *testing.T) {
	s := parseSpec(t, `entities:
  - kind: eoa
    address: "0x1111111111111111111111111111111111111111"`)
	_, _, err := Build(s, BuildOptions{Seed: 0, ClientName: "geth"})
	if err == nil {
		t.Fatal("expected Sizer-required error")
	}
}

func TestBuildRejectsEmptySpec(t *testing.T) {
	if _, _, err := Build(&spec.Spec{}, defaultOpts); err == nil {
		t.Fatal("expected error for empty spec")
	}
}

// TestBuildDeterminismEndToEnd is the strongest determinism guarantee
// the feature ships: same YAML + same seed → byte-identical PreAlloc
// across runs. Pins addresses, account fields, code, AND every
// synthesized storage slot. Without this test, a non-deterministic
// iteration path anywhere in spec→templates→specbuild→materialize
// could silently produce different state across CI runs.
func TestBuildDeterminismEndToEnd(t *testing.T) {
	s, err := spec.ParseFile("../spec/testdata/valid-all-features.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate([]string{"erc20"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	a, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build (run A): %v", err)
	}
	b, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build (run B): %v", err)
	}

	if len(a) != len(b) {
		t.Fatalf("entity count differs across runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Address != b[i].Address {
			t.Errorf("entity[%d] address differs: %s vs %s", i, a[i].Address.Hex(), b[i].Address.Hex())
		}
		if a[i].Account.Nonce != b[i].Account.Nonce {
			t.Errorf("entity[%d] nonce differs: %d vs %d", i, a[i].Account.Nonce, b[i].Account.Nonce)
		}
		if !a[i].Account.Balance.Eq(b[i].Account.Balance) {
			t.Errorf("entity[%d] balance differs: %s vs %s", i, a[i].Account.Balance, b[i].Account.Balance)
		}
		if string(a[i].Account.CodeHash) != string(b[i].Account.CodeHash) {
			t.Errorf("entity[%d] code hash differs", i)
		}
		if string(a[i].Code) != string(b[i].Code) {
			t.Errorf("entity[%d] code bytes differ", i)
		}
		// Compare storage by draining both iterators into maps. Map
		// equality covers content; iteration order is not part of the
		// guarantee (Go maps shuffle; writers sort by keccak before
		// inserting into the MPT, so storage-content equality is what
		// matters for state-root determinism).
		ma := drainStorage(a[i].Storage)
		mb := drainStorage(b[i].Storage)
		if len(ma) != len(mb) {
			t.Errorf("entity[%d] storage slot count differs: %d vs %d", i, len(ma), len(mb))
			continue
		}
		for k, va := range ma {
			vb, ok := mb[k]
			if !ok {
				t.Errorf("entity[%d] key %s missing in run B", i, k.Hex())
				continue
			}
			if va != vb {
				t.Errorf("entity[%d] key %s: run A %s, run B %s", i, k.Hex(), va.Hex(), vb.Hex())
			}
		}
	}
}

// TestBuild_TargetSizeTruncatesSpec pins the cross-client invariance-safe
// truncation: when the projected trie cost of the spec would exceed
// opts.TargetSize, Build returns the longest prefix that fits. Same
// truncation logic runs on every client because BytesPerAccount and
// BytesPerSlot are global constants.
func TestBuild_TargetSizeTruncatesSpec(t *testing.T) {
	// Each EOA entity has ApproximateSizeBytes=0 (no storage), so its
	// projected cost is exactly sizecal.BytesPerAccount. Reading the
	// constant at runtime keeps the test robust to calibration updates.
	const numEntities = 10
	const fitCount = 4
	perEntity := sizecal.BytesPerAccount("geth")
	targetSize := perEntity * fitCount

	var sb strings.Builder
	sb.WriteString("entities:\n")
	for i := 0; i < numEntities; i++ {
		sb.WriteString("  - kind: eoa\n")
		sb.WriteString(fmt.Sprintf("    name: e%d\n", i))
	}
	s := parseSpec(t, sb.String())

	opts := defaultOpts
	opts.TargetSize = targetSize

	pre, diag, err := Build(s, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := len(pre), fitCount; got != want {
		t.Fatalf("entity count after truncation: got %d, want %d", got, want)
	}
	if len(diag.Warnings) == 0 {
		t.Errorf("expected diagnostics warning about truncation; got none")
	}
	foundTrunc := false
	for _, w := range diag.Warnings {
		if strings.Contains(w, "truncated") {
			foundTrunc = true
			break
		}
	}
	if !foundTrunc {
		t.Errorf("expected 'truncated' in diagnostics; got %v", diag.Warnings)
	}

	// Cross-client invariance: every client name truncates at the same index.
	clients := []string{"geth", "reth", "nethermind", "besu"}
	prevLen := -1
	for _, c := range clients {
		o := opts
		o.ClientName = c
		got, _, err := Build(s, o)
		if err != nil {
			t.Fatalf("Build(%s): %v", c, err)
		}
		if prevLen >= 0 && len(got) != prevLen {
			t.Errorf("truncation diverged across clients: prev=%d, %s=%d", prevLen, c, len(got))
		}
		prevLen = len(got)
	}
}

// TestBuild_TargetSizeZeroIsUnlimited pins the contract that TargetSize=0
// means "no truncation" — the default for callers that haven't opted in.
func TestBuild_TargetSizeZeroIsUnlimited(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("entities:\n")
	for i := 0; i < 10; i++ {
		sb.WriteString("  - kind: eoa\n")
		sb.WriteString(fmt.Sprintf("    name: u%d\n", i))
	}
	s := parseSpec(t, sb.String())

	opts := defaultOpts // TargetSize defaults to 0
	pre, diag, err := Build(s, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pre) != 10 {
		t.Errorf("with TargetSize=0 expected all 10 entities, got %d", len(pre))
	}
	for _, w := range diag.Warnings {
		if strings.Contains(w, "truncated") {
			t.Errorf("unexpected truncation warning with TargetSize=0: %s", w)
		}
	}
}

func drainStorage(seq iter.Seq2[common.Hash, common.Hash]) map[common.Hash]common.Hash {
	out := map[common.Hash]common.Hash{}
	if seq == nil {
		return out
	}
	seq(func(k, v common.Hash) bool {
		out[k] = v
		return true
	})
	return out
}
