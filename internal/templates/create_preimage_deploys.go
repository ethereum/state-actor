package templates

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func init() {
	Register(&createPreimageDeploysTemplate{})
}

// createPreimageDeploysTemplate handles
// `kind: contract, template: create_preimage_deploys`. For each nonce
// in [start_nonce, start_nonce+count) it derives the CREATE address
// (keccak256(rlp([sender, nonce]))[12:] via crypto.CreateAddress) and
// plants the derived contract's code there with nonce=1, balance=0,
// and the optional `storage_init` storage.
//
// The CREATE deployer is supplied via the required `sender:` parameter
// — NOT the entity's own resolved address. That keeps the user free to
// also declare a separate entity (e.g. `template: raw`) at the
// sender's address with that account's actual bytecode; `spec.Validate`
// only flags duplicate `address:` fields between entities, so giving
// the sender its own slot avoids that collision.
//
// Backs CreatePreimageLayout in execution-specs bloatnet benchmarks:
// the test code computes keccak256(rlp([sender, nonce]))[12:] at run
// time; this template pre-plants matching state at every such address
// so a single shared `runtime` (the case for Bittrex Controller's
// 1.5M children — all share the same body) becomes the prestate.
//
// Two mutually exclusive code modes (ValidateParameters enforces the
// mutex): literal mode supplies `runtime` verbatim (no EVM simulation;
// shared by every derived contract), and pattern mode names a built-in
// `code_pattern:` generator producing a per-derived-address-unique
// runtime (see code_pattern.go).
type createPreimageDeploysTemplate struct{}

// TemplateNameCreatePreimageDeploys is the registry key for this template.
const TemplateNameCreatePreimageDeploys = "create_preimage_deploys"

func (createPreimageDeploysTemplate) Name() string      { return TemplateNameCreatePreimageDeploys }
func (createPreimageDeploysTemplate) UserVisible() bool { return true }

// HonoredEntityFields: derived contracts are fully described by the
// parameters (nonce 1, balance 0, code from runtime/code_pattern);
// entity-level fields would silently not apply, so they are rejected.
func (createPreimageDeploysTemplate) HonoredEntityFields() EntityFieldSet {
	return EntityFieldSet{
		ApproximateSizeBytes: EntityFieldSupport{Alternative: "parameters.count"},
	}
}

// createPreimageDeploysParams is the typed result of
// parseCreatePreimageDeploysParams.
type createPreimageDeploysParams struct {
	sender      common.Address
	startNonce  uint64
	count       uint64 // in [1, 2^32]
	patternName string // "" in literal runtime mode
	runtime     []byte // shared literal runtime; nil in pattern mode
	storageInit map[common.Hash]common.Hash
}

// parseCreatePreimageDeploysParams is the single validation+parse
// boundary for this template. ValidateParameters and Expand both call
// it, so a parameter set that validates is — by construction — the same
// one that expands; no check can drift between the two entry points.
func parseCreatePreimageDeploysParams(params map[string]any) (createPreimageDeploysParams, error) {
	var pp createPreimageDeploysParams
	if err := RejectUnknownKeys(params, "create_preimage_deploys", []string{
		"sender", "start_nonce", "count", "runtime", "storage_init", "code_pattern",
	}); err != nil {
		return pp, err
	}
	for _, required := range []string{"sender", "count"} {
		if _, ok := params[required]; !ok {
			return pp, fmt.Errorf("create_preimage_deploys: missing required parameter %q", required)
		}
	}
	// `code_pattern` opts into a named per-address runtime generator.
	// When set, `runtime` is forbidden. When unset, `runtime` is
	// required and supplied verbatim.
	if v, has := params["code_pattern"]; has {
		name, ok := v.(string)
		if !ok {
			return pp, fmt.Errorf("create_preimage_deploys: code_pattern must be a string (got %T)", v)
		}
		if !IsKnownCodePattern(name) {
			return pp, fmt.Errorf("create_preimage_deploys: unknown code_pattern %q (known: %q)",
				name, CodePatternUniqueJumpdestPreAmsterdam)
		}
		if _, has := params["runtime"]; has {
			return pp, fmt.Errorf("create_preimage_deploys: `runtime` is forbidden when `code_pattern` is set (the pattern generates per-address runtime)")
		}
		pp.patternName = name
	} else {
		if _, ok := params["runtime"]; !ok {
			return pp, fmt.Errorf("create_preimage_deploys: missing required parameter %q (or set `code_pattern:`)", "runtime")
		}
		runtime, err := ParseHexBytesParam(params["runtime"], "runtime")
		if err != nil {
			return pp, fmt.Errorf("create_preimage_deploys: %w", err)
		}
		if len(runtime) == 0 {
			return pp, fmt.Errorf("create_preimage_deploys: runtime must be non-empty")
		}
		pp.runtime = runtime
	}
	sender, err := ParseAddressParam(params["sender"], "sender")
	if err != nil {
		return pp, fmt.Errorf("create_preimage_deploys: %w", err)
	}
	pp.sender = sender
	count, err := ParseUint64Param(params["count"], "count")
	if err != nil {
		return pp, fmt.Errorf("create_preimage_deploys: %w", err)
	}
	if count == 0 {
		return pp, fmt.Errorf("create_preimage_deploys: count must be >= 1 (count=0 emits nothing; delete the entity instead)")
	}
	if count > practicalFanoutLimit {
		return pp, fmt.Errorf("create_preimage_deploys: count=%d exceeds practical limit (2^32)", count)
	}
	pp.count = count
	// Pattern runtimes are byte-unique per derived address and stay
	// resident for the whole run; cap the estimate at a size no build
	// host survives. (count <= 2^32 and runtime sizes are small, so the
	// product cannot overflow uint64.)
	if pp.patternName != "" {
		if est := count * CodePatternRuntimeSize(pp.patternName); est > patternResidentCodeCapBytes {
			return pp, fmt.Errorf("create_preimage_deploys: code_pattern %q with count=%d would materialize ≈%.0f GiB of unique runtime code in memory (cap %d GiB); split into multiple entities or reduce count",
				pp.patternName, count, float64(est)/float64(1<<30), patternResidentCodeCapBytes>>30)
		}
	}
	if v, has := params["start_nonce"]; has {
		n, err := ParseUint64Param(v, "start_nonce")
		if err != nil {
			return pp, fmt.Errorf("create_preimage_deploys: %w", err)
		}
		pp.startNonce = n
	}
	si, err := ParseStorageInitMap(params["storage_init"])
	if err != nil {
		return pp, fmt.Errorf("create_preimage_deploys: %w", err)
	}
	pp.storageInit = si
	return pp, nil
}

func (createPreimageDeploysTemplate) ValidateParameters(params map[string]any) error {
	_, err := parseCreatePreimageDeploysParams(params)
	return err
}

func (createPreimageDeploysTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	pp, err := parseCreatePreimageDeploysParams(e.Parameters)
	if err != nil {
		return nil, err
	}
	out := make([]PreAllocEntity, 0, pp.count)
	for i := uint64(0); i < pp.count; i++ {
		derived := crypto.CreateAddress(pp.sender, pp.startNonce+i)
		runtime := pp.runtime
		if pp.patternName != "" {
			runtime, err = codePatternRuntimeFor(pp.patternName, derived)
			if err != nil {
				return nil, fmt.Errorf("create_preimage_deploys: nonce=%d: %w", pp.startNonce+i, err)
			}
		}
		out = append(out, PreAllocEntity{
			Address: derived,
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: crypto.Keccak256Hash(runtime).Bytes(),
			},
			Code:    runtime,
			Storage: MapToSeq(pp.storageInit),
		})
	}
	return out, nil
}
