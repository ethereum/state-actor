package templates

import (
	"iter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/state-actor/internal/spec"
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

	// Expand turns one spec entity into 1..N PreAllocEntity records.
	Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error)
}
