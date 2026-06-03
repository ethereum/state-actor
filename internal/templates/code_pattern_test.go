package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestBuildUniqueJumpdestRuntimePreAmsterdamLayout pins the byte-for-byte
// structure of the per-address runtime against the layout documented in
// execution-specs/tests/benchmark/stateful/bloatnet/test_transaction_
// types.py::build_unique_contract_initcode.
func TestBuildUniqueJumpdestRuntimePreAmsterdamLayout(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	got := BuildUniqueJumpdestRuntimePreAmsterdam(addr)

	if len(got) != 0x6000 {
		t.Fatalf("length: got %d, want 0x6000 (24576)", len(got))
	}
	// Entry: PUSH2 0x5FFF; JUMP.
	want := []byte{0x61, 0x5F, 0xFF, 0x56}
	if !bytes.Equal(got[0:4], want) {
		t.Errorf("entry bytes [0:4]: got %x, want %x", got[0:4], want)
	}
	// Bytes 0x04..0x2C: JUMPDEST padding.
	for i := 0x04; i < 0x2C; i++ {
		if got[i] != 0x5B {
			t.Fatalf("byte %d: got 0x%02x, want 0x5B (JUMPDEST padding)", i, got[i])
		}
	}
	// Bytes 0x2C..0x40: embedded address.
	if !bytes.Equal(got[0x2C:0x40], addr[:]) {
		t.Errorf("embedded address [0x2C:0x40]: got %x, want %x", got[0x2C:0x40], addr[:])
	}
	// All remaining bytes through and including 0x5FFF must be JUMPDEST.
	for i := 0x40; i < 0x6000; i++ {
		if got[i] != 0x5B {
			t.Fatalf("byte %d: got 0x%02x, want 0x5B (JUMPDEST filler)", i, got[i])
		}
	}
	// Sentinel: the JUMP target at 0x5FFF must be JUMPDEST so the entry
	// JUMP doesn't revert.
	if got[0x5FFF] != 0x5B {
		t.Errorf("JUMP target byte at 0x5FFF: got 0x%02x, want 0x5B", got[0x5FFF])
	}
}

// TestBuildUniqueJumpdestRuntimePreAmsterdamUnique pins that each
// derived address produces a byte-distinct runtime — the whole point of
// the unique-code-jumpdest pattern.
func TestBuildUniqueJumpdestRuntimePreAmsterdamUnique(t *testing.T) {
	a := BuildUniqueJumpdestRuntimePreAmsterdam(
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
	)
	b := BuildUniqueJumpdestRuntimePreAmsterdam(
		common.HexToAddress("0x0000000000000000000000000000000000000002"),
	)
	if bytes.Equal(a, b) {
		t.Fatalf("two distinct addresses produced byte-identical runtime")
	}
	// They differ only in the embedded-address region (bytes 0x2C..0x40).
	if !bytes.Equal(a[:0x2C], b[:0x2C]) {
		t.Errorf("prefix [0:0x2C] should be identical; differ")
	}
	if !bytes.Equal(a[0x40:], b[0x40:]) {
		t.Errorf("suffix [0x40:] should be identical; differ")
	}
}

// TestBuildUniqueJumpdestInitcodePreAmsterdamShape pins minimum
// invariants on the initcode: non-empty, starts with the seed-JUMPDEST
// PUSH32 prologue, ends with RETURN(0x6000). Full byte-for-byte fidelity
// against the Python source is impractical without an EVM simulator;
// callers that need exact-match validation should use eth_call to
// execute the initcode and compare the returned runtime against
// BuildUniqueJumpdestRuntimePreAmsterdam(ADDRESS).
func TestBuildUniqueJumpdestInitcodePreAmsterdamShape(t *testing.T) {
	ic := BuildUniqueJumpdestInitcodePreAmsterdam()
	if len(ic) == 0 {
		t.Fatal("initcode is empty")
	}
	// Prologue: PUSH32 (32 × JUMPDEST) ; PUSH1 0 ; MSTORE.
	if ic[0] != 0x7F {
		t.Errorf("byte[0]: got 0x%02x, want 0x7F (PUSH32)", ic[0])
	}
	for i := 1; i <= 32; i++ {
		if ic[i] != 0x5B {
			t.Errorf("byte[%d]: got 0x%02x, want 0x5B (JUMPDEST in PUSH32 immediate)", i, ic[i])
		}
	}
	// After the PUSH32 + 32 immediate bytes (33 total): PUSH1 0 ; MSTORE.
	if ic[33] != 0x60 || ic[34] != 0x00 || ic[35] != 0x52 {
		t.Errorf("expected PUSH1 0; MSTORE at bytes 33..35, got %x", ic[33:36])
	}
	// Final 3 bytes: PUSH1 0; RETURN.
	last := ic[len(ic)-3:]
	if last[0] != 0x60 || last[1] != 0x00 || last[2] != 0xF3 {
		t.Errorf("expected PUSH1 0; RETURN as last 3 bytes, got %x", last)
	}
}

// TestPushImmediateEncoding pins the minimal-PUSH encoding the initcode
// builder uses for memory sizes. Includes the boundary at 0xFF/0x100
// where the encoding moves from PUSH1 to PUSH2.
func TestPushImmediateEncoding(t *testing.T) {
	cases := []struct {
		v   uint64
		hex []byte
	}{
		{0, []byte{0x5F}},                       // PUSH0
		{32, []byte{0x60, 0x20}},                // PUSH1 0x20
		{255, []byte{0x60, 0xFF}},               // PUSH1 0xFF
		{256, []byte{0x61, 0x01, 0x00}},         // PUSH2 0x0100
		{16384, []byte{0x61, 0x40, 0x00}},       // PUSH2 0x4000
		{1 << 16, []byte{0x62, 0x01, 0x00, 0x00}}, // PUSH3 0x010000
	}
	for _, c := range cases {
		got := pushImmediate(c.v)
		if !bytes.Equal(got, c.hex) {
			t.Errorf("pushImmediate(%d): got %x, want %x", c.v, got, c.hex)
		}
	}
}

// TestCreate2DeploysCodePattern pins that the create2_deploys template
// generates per-derived-address unique runtime when `code_pattern` is
// set, that the initcode hash drives the CREATE2 derivation, and that
// each derived contract's runtime embeds its OWN derived address.
func TestCreate2DeploysCodePattern(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
		"salt_count":   3,
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("count: got %d, want 3", len(out))
	}
	for i, pe := range out {
		if len(pe.Code) != 0x6000 {
			t.Errorf("derived[%d]: runtime length got %d, want 0x6000", i, len(pe.Code))
		}
		// Embedded address must equal the derived address.
		embedded := common.BytesToAddress(pe.Code[0x2C:0x40])
		if embedded != pe.Address {
			t.Errorf("derived[%d]: embedded address %s != derived %s",
				i, embedded.Hex(), pe.Address.Hex())
		}
		// codeHash must equal keccak256(runtime).
		want := crypto.Keccak256Hash(pe.Code).Bytes()
		if !bytes.Equal(pe.Account.CodeHash, want) {
			t.Errorf("derived[%d]: code-hash mismatch", i)
		}
	}
}

// TestCreate2DeploysCodePatternMutex pins that `code_pattern` is
// mutually exclusive with both `initcode` and `runtime`.
func TestCreate2DeploysCodePatternMutex(t *testing.T) {
	tmpl := &create2DeploysTemplate{}
	cases := []map[string]any{
		{
			"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
			"salt_count":   1,
			"initcode":     "0xfe",
		},
		{
			"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
			"salt_count":   1,
			"runtime":      "0xfe",
		},
		{
			"code_pattern": "no_such_pattern",
			"salt_count":   1,
		},
		{
			"code_pattern": 1, // wrong type
			"salt_count":   1,
		},
	}
	for i, c := range cases {
		if err := tmpl.ValidateParameters(c); err == nil {
			t.Errorf("case[%d]: expected validation error, got nil for %v", i, c)
		}
	}
}

// TestCreatePreimageDeploysCodePattern pins the same per-address-
// uniqueness on the CREATE-preimage template. CREATE doesn't hash the
// initcode for address derivation, so the test just checks that each
// derived contract gets a 24576-byte runtime with its own address
// embedded.
func TestCreatePreimageDeploysCodePattern(t *testing.T) {
	ent := mkContractEntity("create_preimage_deploys", map[string]any{
		"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
		"sender":       bittrexAddrHex,
		"start_nonce":  2,
		"count":        3,
	})
	out, err := (&createPreimageDeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("count: got %d, want 3", len(out))
	}
	for i, pe := range out {
		if len(pe.Code) != 0x6000 {
			t.Errorf("derived[%d]: runtime length got %d, want 0x6000", i, len(pe.Code))
		}
		embedded := common.BytesToAddress(pe.Code[0x2C:0x40])
		if embedded != pe.Address {
			t.Errorf("derived[%d]: embedded address %s != derived %s",
				i, embedded.Hex(), pe.Address.Hex())
		}
	}
}

// TestCreatePreimageDeploysCodePatternMutex pins `code_pattern` ↔
// `runtime` mutex on the create_preimage_deploys side.
func TestCreatePreimageDeploysCodePatternMutex(t *testing.T) {
	tmpl := &createPreimageDeploysTemplate{}
	cases := []map[string]any{
		{
			"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
			"sender":       bittrexAddrHex,
			"count":        1,
			"runtime":      "0xfe",
		},
		{
			"code_pattern": "bogus",
			"sender":       bittrexAddrHex,
			"count":        1,
		},
	}
	for i, c := range cases {
		if err := tmpl.ValidateParameters(c); err == nil {
			t.Errorf("case[%d]: expected validation error, got nil for %v", i, c)
		}
	}
}
