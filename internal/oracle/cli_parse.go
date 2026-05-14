package oracle

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/holiman/uint256"
)

// ParseBalance accepts plain wei integers or human-readable suffixes
// ("ether", "gwei", "wei", "eth"). Examples: "1ether", "100gwei",
// "1000000000000000000". The result is in wei.
//
// Suffix parsing is permissive on whitespace and case ("1 Ether" works).
// Returns an error for negative numbers, malformed input, or overflow
// past uint256 max.
func ParseBalance(s string) (*uint256.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("ParseBalance: empty input")
	}
	lower := strings.ToLower(s)

	// Suffix table: longest match first so "ether" doesn't match before "eth".
	suffixes := []struct {
		name string
		mult string // decimal multiplier vs wei
	}{
		{"ether", "1000000000000000000"},
		{"eth", "1000000000000000000"},
		{"gwei", "1000000000"},
		{"wei", "1"},
	}

	var numStr string
	mult := big.NewInt(1)
	matched := false
	for _, sf := range suffixes {
		if strings.HasSuffix(lower, sf.name) {
			numStr = strings.TrimSpace(lower[:len(lower)-len(sf.name)])
			m, ok := new(big.Int).SetString(sf.mult, 10)
			if !ok {
				return nil, fmt.Errorf("ParseBalance: internal bad multiplier %q", sf.mult)
			}
			mult = m
			matched = true
			break
		}
	}
	if !matched {
		numStr = lower
	}

	if numStr == "" {
		return nil, fmt.Errorf("ParseBalance: missing number in %q", s)
	}

	// Allow "0x" hex form as well, for completeness.
	var n *big.Int
	if strings.HasPrefix(numStr, "0x") {
		var ok bool
		n, ok = new(big.Int).SetString(numStr[2:], 16)
		if !ok {
			return nil, fmt.Errorf("ParseBalance: bad hex number %q", numStr)
		}
	} else {
		var ok bool
		n, ok = new(big.Int).SetString(numStr, 10)
		if !ok {
			return nil, fmt.Errorf("ParseBalance: bad number %q", numStr)
		}
	}
	if n.Sign() < 0 {
		return nil, fmt.Errorf("ParseBalance: negative balance %q", s)
	}
	n.Mul(n, mult)
	out, overflow := uint256.FromBig(n)
	if overflow {
		return nil, fmt.Errorf("ParseBalance: value %s overflows uint256", n)
	}
	return out, nil
}

// ParseCREATE2DeploySpec parses a --create2-deploy flag value of the form
// "initcode=0x<hex>,salt-start=<u64>,salt-count=<u64>[,deployed-code=0x<hex>]".
// Keys are case-insensitive; whitespace around keys/values is trimmed.
// All three of initcode, salt-start, salt-count are required.
func ParseCREATE2DeploySpec(s string) (CREATE2DeploySpec, error) {
	var spec CREATE2DeploySpec
	if strings.TrimSpace(s) == "" {
		return spec, fmt.Errorf("ParseCREATE2DeploySpec: empty spec")
	}

	seen := map[string]bool{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			return spec, fmt.Errorf("ParseCREATE2DeploySpec: missing '=' in %q", kv)
		}
		key := strings.ToLower(strings.TrimSpace(kv[:eq]))
		val := strings.TrimSpace(kv[eq+1:])
		if seen[key] {
			return spec, fmt.Errorf("ParseCREATE2DeploySpec: duplicate key %q", key)
		}
		seen[key] = true

		switch key {
		case "initcode":
			b, err := decodeHex(val)
			if err != nil {
				return spec, fmt.Errorf("ParseCREATE2DeploySpec: initcode: %w", err)
			}
			spec.Initcode = b
		case "salt-start":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return spec, fmt.Errorf("ParseCREATE2DeploySpec: salt-start: %w", err)
			}
			spec.SaltStart = n
		case "salt-count":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return spec, fmt.Errorf("ParseCREATE2DeploySpec: salt-count: %w", err)
			}
			spec.SaltCount = n
		case "deployed-code":
			b, err := decodeHex(val)
			if err != nil {
				return spec, fmt.Errorf("ParseCREATE2DeploySpec: deployed-code: %w", err)
			}
			spec.DeployedCode = b
		default:
			return spec, fmt.Errorf("ParseCREATE2DeploySpec: unknown key %q (allowed: initcode, salt-start, salt-count, deployed-code)", key)
		}
	}
	if !seen["initcode"] {
		return spec, fmt.Errorf("ParseCREATE2DeploySpec: missing required key 'initcode'")
	}
	if !seen["salt-count"] {
		return spec, fmt.Errorf("ParseCREATE2DeploySpec: missing required key 'salt-count'")
	}
	// salt-start defaults to 0 if omitted — accept that for ergonomics.
	return spec, nil
}

// decodeHex tolerates an optional "0x"/"0X" prefix and empty input.
func decodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s = s[2:]
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd-length hex %q", s)
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := fromHexNibble(s[i*2])
		lo, ok2 := fromHexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex byte at offset %d", i*2)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func fromHexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
