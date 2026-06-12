package templates

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

// Generic parameter helpers for template ValidateParameters/Expand.
// The erc20 template has its own copies with "erc20:" error prefixes;
// these versions are template-neutral so the caller (a new template)
// owns the prefix in the wrapping fmt.Errorf.

// practicalFanoutLimit caps every fan-out knob (sequential_eoas.count,
// sequential_pkey_eoas.count, create2_deploys.salt_count,
// create_preimage_deploys.count, storage_pattern.final) at 2^32 units.
// Anything larger is almost certainly a typo — and for storage_pattern
// the cap is also load-bearing for correctness: the slot iterator's
// `k <= final` loop cannot terminate at final == MaxUint64, and slot 0
// (final+1) would silently wrap to 0.
const practicalFanoutLimit = uint64(1) << 32

// ParseAddressParam decodes a quoted 0x-prefixed 20-byte hex string.
func ParseAddressParam(v any, label string) (common.Address, error) {
	s, ok := v.(string)
	if !ok {
		return common.Address{}, fmt.Errorf("%s must be a quoted hex string (got %T)", label, v)
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return common.Address{}, fmt.Errorf("%s must have 0x prefix (got %q)", label, s)
	}
	if len(s) != 2+2*common.AddressLength {
		return common.Address{}, fmt.Errorf("%s must be 20 bytes (42 hex chars including 0x), got %d chars in %q",
			label, len(s), s)
	}
	raw, err := hex.DecodeString(s[2:])
	if err != nil {
		return common.Address{}, fmt.Errorf("%s decode %q: %w", label, s, err)
	}
	var addr common.Address
	copy(addr[:], raw)
	return addr, nil
}

// ParseUint256Param decodes a quoted decimal or 0x-hex string. Empty
// string is rejected. Delegates to spec.ParseUint256 so rules match
// the entity-level `balance:` field.
func ParseUint256Param(v any, label string) (*uint256.Int, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a quoted decimal or 0x-hex string (got %T)", label, v)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%s is empty", label)
	}
	u, err := spec.ParseUint256(s)
	if err != nil {
		return nil, fmt.Errorf("%s decode %q: %w", label, s, err)
	}
	return u, nil
}

// ParseHexBytesParam decodes a quoted hex string (optional 0x prefix)
// into bytes. Empty string returns (nil, nil). Used for initcode,
// deployed_code, runtime code parameters.
func ParseHexBytesParam(v any, label string) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a quoted hex string (got %T)", label, v)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s = s[2:]
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("%s odd-length hex (%d chars)", label, len(s))
	}
	out, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%s decode: %w", label, err)
	}
	return out, nil
}

// ParseUint64Param decodes a non-negative integer parameter into uint64.
// Accepts int / int64 / uint64 (yaml.v3's int decoders).
func ParseUint64Param(v any, label string) (uint64, error) {
	switch n := v.(type) {
	case int:
		if n < 0 {
			return 0, fmt.Errorf("%s must be >= 0 (got %d)", label, n)
		}
		return uint64(n), nil
	case int64:
		if n < 0 {
			return 0, fmt.Errorf("%s must be >= 0 (got %d)", label, n)
		}
		return uint64(n), nil
	case uint64:
		return n, nil
	default:
		return 0, fmt.Errorf("%s must be a non-negative integer (got %T)", label, v)
	}
}

// ParseStorageInitMap decodes the YAML form
//
//	storage_init:
//	  "0x0":  "0xa3c1e324ca1ce40db73ed6026c4a177f099b5770"
//	  "0x1":  "0x..."
//
// into a typed `map[common.Hash]common.Hash`. Each slot key and each
// value is treated as a 32-byte hash: shorter hex strings are
// left-padded with zero bytes, longer ones are rejected. Quoted-string
// form is mandatory because yaml.v3 decodes nested maps as
// `map[string]any` and our top-level scalar hooks don't apply inside
// `parameters:`.
//
// Two keys that decode to the same canonical hash (e.g. `"0x0"` and
// `"0x00"`) are rejected as a duplicate, because the user almost
// certainly meant two distinct slots.
//
// Returns `(nil, nil)` when the input is nil or an empty map so the
// caller can treat "no storage to plant" uniformly.
func ParseStorageInitMap(v any) (map[common.Hash]common.Hash, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("storage_init must be a map of hex-string slot → hex-string value (got %T)", v)
	}
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[common.Hash]common.Hash, len(m))
	rawKeys := make(map[common.Hash]string, len(m))
	for slotStr, valAny := range m {
		slot, err := parseHashString(slotStr, "storage_init slot")
		if err != nil {
			return nil, err
		}
		if prev, dup := rawKeys[slot]; dup {
			return nil, fmt.Errorf("storage_init: keys %q and %q decode to the same slot %s", prev, slotStr, slot.Hex())
		}
		rawKeys[slot] = slotStr
		valStr, ok := valAny.(string)
		if !ok {
			return nil, fmt.Errorf("storage_init[%q]: value must be a quoted hex string (got %T)", slotStr, valAny)
		}
		val, err := parseHashString(valStr, fmt.Sprintf("storage_init[%q] value", slotStr))
		if err != nil {
			return nil, err
		}
		out[slot] = val
	}
	return out, nil
}

// parseHashString decodes a hex string into a left-padded 32-byte hash.
// Accepts an optional 0x/0X prefix; rejects empty, non-hex, and
// strings whose decoded length exceeds 32 bytes.
func parseHashString(s, label string) (common.Hash, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return common.Hash{}, fmt.Errorf("%s: empty hex string", label)
	}
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	if len(s) > 64 {
		return common.Hash{}, fmt.Errorf("%s: hex value exceeds 32 bytes (got %d hex chars)", label, len(s))
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return common.Hash{}, fmt.Errorf("%s: decode error: %w", label, err)
	}
	var h common.Hash
	copy(h[common.HashLength-len(raw):], raw)
	return h, nil
}

// RejectUnknownKeys errors when params contains a key outside allowed.
// The template name is interpolated into the error to point users at
// the right schema docs.
func RejectUnknownKeys(params map[string]any, template string, allowed []string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	for k := range params {
		if _, ok := set[k]; !ok {
			return fmt.Errorf("%s: unknown parameter %q (allowed: %v)", template, k, allowed)
		}
	}
	return nil
}
