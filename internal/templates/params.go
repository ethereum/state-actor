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
