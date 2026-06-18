package templates

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func init() {
	Register(&create2DeploysTemplate{})
}

// create2DeploysTemplate handles `kind: contract, template: create2_deploys`.
// For each salt in [salt_start, salt_start+salt_count), derives a
// CREATE2 address (keccak256(0xff ++ factory ++ salt ++ keccak256(initcode))[12:])
// and plants `runtime` there with nonce=1, optionally with the
// per-derived-address storage prefilled from `storage_init`.
//
// The entity's resolved address is NOT used as an emission address —
// the template only emits at the derived addresses. The entity can
// still be named or anchored to a deterministic anchor address via
// `name:` / `address:` if the user wants reproducible spec ordering.
//
// Two mutually exclusive code modes (ValidateParameters enforces the
// mutex):
//
//   - literal mode: `initcode` (hashed for the CREATE2 derivation only;
//     the constructor is never executed) plus `runtime` (supplied
//     verbatim and shared by every derived contract — the template does
//     not run the EVM).
//   - pattern mode: `code_pattern:` names a built-in generator that owns
//     BOTH the constant initcode and a per-derived-address-unique
//     runtime (e.g. the ADDRESS-dependent unique-jumpdest layout; see
//     code_pattern.go).
//
// Symmetric with `create_preimage_deploys`: the only difference between
// the two templates is the address-derivation algorithm; all other
// account properties (`runtime`, `storage_init`, etc.) behave
// identically.
type create2DeploysTemplate struct{}

// TemplateNameCreate2Deploys is the registry key for this template.
// specbuild's Arachnid-pairing enforcement matches entities against it;
// Name() returns this constant so the two can never drift.
const TemplateNameCreate2Deploys = "create2_deploys"

func (create2DeploysTemplate) Name() string      { return TemplateNameCreate2Deploys }
func (create2DeploysTemplate) UserVisible() bool { return true }

// HonoredEntityFields: derived contracts are fully described by the
// parameters (nonce 1, balance 0, code from runtime/code_pattern);
// entity-level fields would silently not apply, so they are rejected.
func (create2DeploysTemplate) HonoredEntityFields() EntityFieldSet {
	return EntityFieldSet{
		ApproximateSizeBytes: EntityFieldSupport{Alternative: "parameters.salt_count"},
	}
}

// create2DeploysParams is the typed result of parseCreate2DeploysParams.
type create2DeploysParams struct {
	saltStart   uint64
	saltCount   uint64         // in [1, 2^32]
	factory     common.Address // defaults to CanonicalCREATE2FactoryAddress
	patternName string         // "" in literal initcode/runtime mode
	initcode    []byte         // literal parameter, or the pattern-owned constant
	runtime     []byte         // shared literal runtime; nil in pattern mode
	storageInit map[common.Hash]common.Hash
}

// parseCreate2DeploysParams is the single validation+parse boundary for
// this template. ValidateParameters and Expand both call it, so a
// parameter set that validates is — by construction — the same one that
// expands; no check can drift between the two entry points.
func parseCreate2DeploysParams(params map[string]any) (create2DeploysParams, error) {
	var pp create2DeploysParams
	if err := RejectUnknownKeys(params, "create2_deploys", []string{
		"initcode", "salt_start", "salt_count", "runtime", "factory", "storage_init", "code_pattern",
	}); err != nil {
		return pp, err
	}
	// salt_count is always required.
	if _, ok := params["salt_count"]; !ok {
		return pp, fmt.Errorf("create2_deploys: missing required parameter %q", "salt_count")
	}
	// `code_pattern` opts into a named generator (initcode + per-address
	// runtime). When set, `initcode` and `runtime` are forbidden — the
	// pattern owns both. When unset, both are required and supplied
	// verbatim.
	if v, has := params["code_pattern"]; has {
		name, ok := v.(string)
		if !ok {
			return pp, fmt.Errorf("create2_deploys: code_pattern must be a string (got %T)", v)
		}
		if !IsKnownCodePattern(name) {
			return pp, fmt.Errorf("create2_deploys: unknown code_pattern %q (known: %q)",
				name, CodePatternUniqueJumpdestPreAmsterdam)
		}
		if _, has := params["initcode"]; has {
			return pp, fmt.Errorf("create2_deploys: `initcode` is forbidden when `code_pattern` is set (the pattern owns the initcode)")
		}
		if _, has := params["runtime"]; has {
			return pp, fmt.Errorf("create2_deploys: `runtime` is forbidden when `code_pattern` is set (the pattern generates per-address runtime)")
		}
		pp.patternName = name
		initcode, err := codePatternInitcodeFor(name)
		if err != nil {
			return pp, fmt.Errorf("create2_deploys: %w", err)
		}
		pp.initcode = initcode
	} else {
		for _, required := range []string{"initcode", "runtime"} {
			if _, ok := params[required]; !ok {
				return pp, fmt.Errorf("create2_deploys: missing required parameter %q (or set `code_pattern:`)", required)
			}
		}
		initcode, err := ParseHexBytesParam(params["initcode"], "initcode")
		if err != nil {
			return pp, fmt.Errorf("create2_deploys: %w", err)
		}
		if len(initcode) == 0 {
			return pp, fmt.Errorf("create2_deploys: initcode must be non-empty")
		}
		runtime, err := ParseHexBytesParam(params["runtime"], "runtime")
		if err != nil {
			return pp, fmt.Errorf("create2_deploys: %w", err)
		}
		if len(runtime) == 0 {
			return pp, fmt.Errorf("create2_deploys: runtime must be non-empty (this template does not run the EVM; supply the runtime bytecode directly)")
		}
		pp.initcode = initcode
		pp.runtime = runtime
	}
	saltCount, err := ParseUint64Param(params["salt_count"], "salt_count")
	if err != nil {
		return pp, fmt.Errorf("create2_deploys: %w", err)
	}
	if saltCount == 0 {
		return pp, fmt.Errorf("create2_deploys: salt_count must be >= 1 (salt_count=0 emits nothing; delete the entity instead)")
	}
	if saltCount > practicalFanoutLimit {
		return pp, fmt.Errorf("create2_deploys: salt_count=%d exceeds practical limit (2^32)", saltCount)
	}
	pp.saltCount = saltCount
	// Pattern runtimes are byte-unique per derived address and stay
	// resident for the whole run; cap the estimate at a size no build
	// host survives. (saltCount <= 2^32 and runtime sizes are small, so
	// the product cannot overflow uint64.)
	if pp.patternName != "" {
		if est := saltCount * CodePatternRuntimeSize(pp.patternName); est > patternResidentCodeCapBytes {
			return pp, fmt.Errorf("create2_deploys: code_pattern %q with salt_count=%d would materialize ≈%.0f GiB of unique runtime code in memory (cap %d GiB); split into multiple entities or reduce salt_count",
				pp.patternName, saltCount, float64(est)/float64(1<<30), patternResidentCodeCapBytes>>30)
		}
	}
	if v, has := params["salt_start"]; has {
		s, err := ParseUint64Param(v, "salt_start")
		if err != nil {
			return pp, fmt.Errorf("create2_deploys: %w", err)
		}
		pp.saltStart = s
	}
	pp.factory = CanonicalCREATE2FactoryAddress
	if v, has := params["factory"]; has {
		f, err := ParseAddressParam(v, "factory")
		if err != nil {
			return pp, fmt.Errorf("create2_deploys: %w", err)
		}
		pp.factory = f
	}
	si, err := ParseStorageInitMap(params["storage_init"])
	if err != nil {
		return pp, fmt.Errorf("create2_deploys: %w", err)
	}
	pp.storageInit = si
	return pp, nil
}

func (create2DeploysTemplate) ValidateParameters(params map[string]any) error {
	_, err := parseCreate2DeploysParams(params)
	return err
}

func (create2DeploysTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	pp, err := parseCreate2DeploysParams(e.Parameters)
	if err != nil {
		return nil, err
	}

	initHash := crypto.Keccak256(pp.initcode)
	out := make([]PreAllocEntity, 0, pp.saltCount)
	for k := uint64(0); k < pp.saltCount; k++ {
		var salt [32]byte
		binary.BigEndian.PutUint64(salt[24:], pp.saltStart+k)
		derived := crypto.CreateAddress2(pp.factory, salt, initHash)

		runtime := pp.runtime
		if pp.patternName != "" {
			runtime, err = codePatternRuntimeFor(pp.patternName, derived)
			if err != nil {
				return nil, fmt.Errorf("create2_deploys: salt=%d: %w", pp.saltStart+k, err)
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
