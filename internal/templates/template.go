package templates

import (
	"fmt"
	"iter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/nerolation/state-actor/internal/spec"
)

// PreAllocEntity is the unified post-expansion record every writer
// consumes. Storage is iter.Seq2 (not materialised) so multi-GB
// entities don't OOM; templates emit deterministically and writers
// sort by keccak(key).
type PreAllocEntity struct {
	// Address is determined by internal/specbuild/derive.go.
	Address common.Address

	// Account is the StateAccount header. Templates set Nonce + Balance;
	// CodeHash iff Code is non-nil; Root is left as EmptyRootHash and
	// computed by the writer from Storage.
	Account *types.StateAccount

	// Code is the deployed bytecode. nil for plain EOAs; can be a
	// 23-byte EIP-7702 0xef0100<addr> delegation marker for EOAs.
	Code []byte

	// Storage yields (key, value) pairs. nil when the entity has no
	// storage. Iteration order is not keccak-sorted; the consumer sorts.
	// Pure-function iterators are re-iterable.
	Storage iter.Seq2[common.Hash, common.Hash]
}

// Context carries the inputs a Template needs to deterministically
// expand one spec entity into one or more PreAllocEntity records.
type Context struct {
	// Seed is the run-wide RNG seed; templates that synthesize addresses
	// or storage MUST derive from it for byte-identical reruns.
	Seed int64

	// ClientName identifies the consuming writer; used by Sizer for the
	// per-client bytes-per-slot calibration.
	ClientName string

	// Sizer translates approximate_size_bytes into a slot count.
	Sizer SizeApproximator

	// ResolvedAddress is the entity's address (decided by the translator
	// at internal/specbuild/derive.go).
	ResolvedAddress common.Address

	// EntityIndex is the 0-based position in the spec; templates use it
	// to disambiguate synthesis keys.
	EntityIndex int
}

// SizeApproximator converts a target byte budget to a synthetic slot
// count. Defined here to avoid a cycle with internal/sizecal.
type SizeApproximator interface {
	SlotsForBytes(client string, targetBytes uint64) int
}

// Template is the extension point: one new file implementing this
// interface plus Register() in init() adds a new template.
//
// Determinism contract: for the same (ctx, e), Expand returns the same
// []PreAllocEntity byte-for-byte across runs and machines.
type Template interface {
	Name() string

	// UserVisible reports whether the template is exposed via the YAML
	// `template:` field. Internal-only templates (raw, eoa) return false.
	UserVisible() bool

	// ValidateParameters runs at parse time so unknown parameter keys
	// surface as user errors early. Implementations should reject typos.
	ValidateParameters(params map[string]any) error

	// HonoredEntityFields declares which entity-level fields (balance,
	// nonce, code, approximate_size_bytes) Expand reads.
	// specbuild.Build rejects a spec entity that sets a field its
	// template ignores, so user-declared state never silently
	// disappears from the generated prestate.
	HonoredEntityFields() EntityFieldSet

	// Expand turns one spec entity into 1..N PreAllocEntity records.
	Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error)
}

// EntityFieldSupport describes a template's handling of one entity-level
// YAML field.
type EntityFieldSupport struct {
	// Honored reports whether Expand reads the field.
	Honored bool
	// Alternative, for ignored fields, names the parameters-level
	// replacement to suggest in the rejection error (e.g.
	// "parameters.balance"). Empty means there is no replacement; the
	// user must delete the field.
	Alternative string
}

// EntityFieldSet declares which entity-level fields a template honors.
// The zero value means "ignored, no alternative" for every field.
// `address:` and `name:` are deliberately not represented: address
// anchoring and stable spec ordering are universal concerns resolved by
// internal/specbuild before Expand runs.
type EntityFieldSet struct {
	Balance              EntityFieldSupport
	Nonce                EntityFieldSupport
	Code                 EntityFieldSupport
	ApproximateSizeBytes EntityFieldSupport
}

// AllEntityFieldsHonored is the declaration for templates (eoa, raw)
// that honor every entity-level field.
func AllEntityFieldsHonored() EntityFieldSet {
	h := EntityFieldSupport{Honored: true}
	return EntityFieldSet{Balance: h, Nonce: h, Code: h, ApproximateSizeBytes: h}
}

// CheckEntityFields returns an error when e sets an entity-level field
// that t declares ignored. nonce: 0, empty code:, and
// approximate_size_bytes: 0 are indistinguishable from "unset" (Go zero
// values) and are never flagged; balance is a pointer, so an explicit
// `balance: "0"` on an ignoring template IS rejected.
func CheckEntityFields(t Template, e spec.Entity) error {
	h := t.HonoredEntityFields()
	checks := []struct {
		set   bool
		field string
		sup   EntityFieldSupport
	}{
		{e.Balance != nil, "balance", h.Balance},
		{e.Nonce != 0, "nonce", h.Nonce},
		{len(e.Code) > 0, "code", h.Code},
		{e.ApproximateSizeBytes != 0, "approximate_size_bytes", h.ApproximateSizeBytes},
	}
	for _, c := range checks {
		if !c.set || c.sup.Honored {
			continue
		}
		if c.sup.Alternative != "" {
			return fmt.Errorf("%s: entity-level `%s:` is ignored by this template — use %s instead",
				t.Name(), c.field, c.sup.Alternative)
		}
		return fmt.Errorf("%s: entity-level `%s:` is ignored by this template — remove it",
			t.Name(), c.field)
	}
	return nil
}
