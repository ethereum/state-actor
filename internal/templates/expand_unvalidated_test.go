package templates

import (
	"math"
	"testing"
)

// These tests pin the parse-once contract (review findings I1/I2): Expand
// performs the same validation as ValidateParameters, so garbage
// parameters reaching Expand directly — programmatic callers bypassing
// the validator — fail loudly instead of silently degrading (zero
// entities, zero-address factory, codeless contracts, nil-balance
// panic: the commit-1362de0 silent-parse pattern).

func TestCREATE2DeploysExpandRejectsUnvalidatedParams(t *testing.T) {
	tmpl := &create2DeploysTemplate{}
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"string salt_count (was: silent zero-entity build)", map[string]any{
			"initcode": "0xfe", "runtime": "0x00", "salt_count": "10",
		}},
		{"malformed factory (was: derivation from the zero address)", map[string]any{
			"initcode": "0xfe", "runtime": "0x00", "salt_count": 1, "factory": "not-hex",
		}},
		{"non-string code_pattern", map[string]any{
			"salt_count": 1, "code_pattern": 5,
		}},
		{"bad salt_start type", map[string]any{
			"initcode": "0xfe", "runtime": "0x00", "salt_count": 1, "salt_start": "7",
		}},
	}
	for _, c := range cases {
		out, err := tmpl.Expand(Context{}, mkContractEntity("create2_deploys", c.params))
		if err == nil {
			t.Errorf("%s: Expand returned nil error (out=%d entities)", c.name, len(out))
		}
	}
}

func TestCreatePreimageDeploysExpandRejectsUnvalidatedParams(t *testing.T) {
	tmpl := &createPreimageDeploysTemplate{}
	sender := "0x000000000000000000000000000000000000beef"
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"non-string code_pattern (was: silent codeless contracts)", map[string]any{
			"sender": sender, "count": 1, "code_pattern": 5,
		}},
		{"non-string sender (was: derivation from the zero address)", map[string]any{
			"sender": 12345, "count": 1, "runtime": "0x00",
		}},
		{"bad start_nonce type", map[string]any{
			"sender": sender, "count": 1, "runtime": "0x00", "start_nonce": "2",
		}},
		{"string count (was: silent zero-entity build)", map[string]any{
			"sender": sender, "count": "5", "runtime": "0x00",
		}},
	}
	for _, c := range cases {
		out, err := tmpl.Expand(Context{}, mkContractEntity("create_preimage_deploys", c.params))
		if err == nil {
			t.Errorf("%s: Expand returned nil error (out=%d entities)", c.name, len(out))
		}
	}
}

func TestSequentialPkeyEOAsExpandRejectsUnvalidatedParams(t *testing.T) {
	tmpl := &sequentialPkeyEOAsTemplate{}
	pkey := "0x1111111111111111111111111111111111111111111111111111111111111111"
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"non-string start_pkey", map[string]any{
			"start_pkey": 7, "count": 1,
		}},
		{"string count (was: silent zero-entity build)", map[string]any{
			"start_pkey": pkey, "count": "5",
		}},
		{"non-string balance (was: nil-deref panic)", map[string]any{
			"start_pkey": pkey, "count": 1, "balance": 7,
		}},
		{"short pkey (was: silent derivation from a 1-byte scalar)", map[string]any{
			"start_pkey": "0x11", "count": 1,
		}},
	}
	for _, c := range cases {
		out, err := tmpl.Expand(Context{}, mkContractEntity("sequential_pkey_eoas", c.params))
		if err == nil {
			t.Errorf("%s: Expand returned nil error (out=%d entities)", c.name, len(out))
		}
	}
}

func TestSequentialEOAsExpandRejectsUnvalidatedParams(t *testing.T) {
	tmpl := &sequentialEOAsTemplate{}
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"string count", map[string]any{"count": "100"}},
		{"non-string balance", map[string]any{"count": 1, "balance": 7}},
	}
	for _, c := range cases {
		out, err := tmpl.Expand(Context{}, mkContractEntity("sequential_eoas", c.params))
		if err == nil {
			t.Errorf("%s: Expand returned nil error (out=%d entities)", c.name, len(out))
		}
	}
}

// TestStoragePatternRejectsHugeFinal pins the 2^32 cap on `final`
// (review finding I2). Load-bearing, not just hygiene: yaml.v3 delivers
// 18446744073709551615 as uint64, ParseUint64Param accepts it,
// storagePatternIter's `k <= final` loop never terminates at MaxUint64,
// and slot 0 (final+1) wraps to 0. The test must NEVER range the
// iterator — it asserts rejection at both entry points instead.
func TestStoragePatternRejectsHugeFinal(t *testing.T) {
	tmpl := &storagePatternTemplate{}
	for _, final := range []uint64{uint64(1)<<32 + 1, math.MaxUint64} {
		if err := tmpl.ValidateParameters(map[string]any{"final": final}); err == nil {
			t.Errorf("ValidateParameters(final=%d): nil error (slot 0 wraps to 0 at MaxUint64 and the storage iterator never terminates)", final)
		}
		if _, err := tmpl.Expand(Context{}, mkContractEntity("storage_pattern", map[string]any{"final": final})); err == nil {
			t.Errorf("Expand(final=%d): nil error", final)
		}
	}
}
