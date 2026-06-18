package templates

import (
	"bytes"
	"strings"
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

// TestBuildUniqueJumpdestInitcodeMatchesEEST pins the keccak256 of the
// state-actor-emitted initcode to the value EEST's
// `build_unique_contract_initcode()` produces. State-actor's CREATE2
// derivation uses keccak256(initcode) as the third CREATE2 input, so
// the planted contract addresses MUST line up with what
// execution-specs `yield_distinct_unique_code_jumpdest_receiver()`
// queries — otherwise tests call empty addresses and benchmark gas
// drifts by the contract's body cost × iteration count.
//
// Pinned value verified against the live EEST snapshot at
// tests/benchmark/stateful/bloatnet/test_transaction_types.py;
// regenerate this constant from Python via:
//
//   import sys; sys.path.insert(0, 'tests/benchmark/stateful/bloatnet')
//   from test_transaction_types import JOCHEMNET_UNIQUE_CONTRACT_INITCODE
//   from Crypto.Hash import keccak
//   print(keccak.new(digest_bits=256, data=JOCHEMNET_UNIQUE_CONTRACT_INITCODE).hexdigest())
//
// If EEST changes the Python initcode shape, update this constant in
// lockstep — both sides must always agree byte-for-byte.
func TestBuildUniqueJumpdestInitcodeMatchesEEST(t *testing.T) {
	const eestInitcodeKeccak = "0xb9cdb9047474294c9743cf3944156c844bf91763de66271493caa07a3de77ec5"
	ic := BuildUniqueJumpdestInitcodePreAmsterdam()
	got := crypto.Keccak256Hash(ic).Hex()
	if got != eestInitcodeKeccak {
		t.Fatalf(
			"initcode keccak diverged from EEST: got %s want %s\n"+
				"  state-actor and execution-specs derive CREATE2 addresses\n"+
				"  from this initcode; a mismatch means planted contracts\n"+
				"  live at addresses the tests don't query (empty-account\n"+
				"  calls during fill-stateful, ~12-gas-per-iteration shortfall\n"+
				"  in test_ether_transfers_onchain_receivers[*unique_code_jumpdest*]).",
			got, eestInitcodeKeccak,
		)
	}
}

// TestCreate2DeploysPatternAddressesMatchEEST pins two DERIVED ADDRESSES
// (not just the initcode hash) to literals computed outside go-ethereum.
// Tuple: factory = canonical Arachnid (the default), salts 0 and 1
// (big-endian in the low 8 bytes of the 32-byte salt), initcode =
// unique_jumpdest_pre_amsterdam pattern. EEST's
// yield_distinct_unique_code_jumpdest_receiver() must resolve to these
// same addresses or the benchmark calls empty accounts.
//
// The initcode keccak is EEST-pinned above; this test additionally pins
// the salt int → bytes32 encoding convention, the last repo-internal
// link in the derivation chain. Regenerate from an execution-specs
// checkout:
//
//	import sys; sys.path.insert(0, 'tests/benchmark/stateful/bloatnet')
//	from test_transaction_types import JOCHEMNET_UNIQUE_CONTRACT_INITCODE
//	from ethereum_test_tools import compute_create2_address
//	print(compute_create2_address(0x4e59b44847b379578588920cA78FbF26c0B4956C, 0,
//	      JOCHEMNET_UNIQUE_CONTRACT_INITCODE))
//
// Fallback derivation (used to produce these pins; two independent
// keccak implementations — pycryptodome and eth_hash — agree):
//
//	keccak256(0xff ++ 4e59…956c ++ uint256(salt) ++
//	          b9cdb9047474294c9743cf3944156c844bf91763de66271493caa07a3de77ec5)[12:]
func TestCreate2DeploysPatternAddressesMatchEEST(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
		"salt_count":   2,
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("count: got %d, want 2", len(out))
	}
	want := []common.Address{
		common.HexToAddress("0xEC9B57721DeaF87D94C5aB94E3c919c60b42aAd8"), // salt 0
		common.HexToAddress("0x59B3D48aEA1b74c1BD4850f471aD067A8c3bc5Aa"), // salt 1
	}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("salt=%d: got %s, want EEST-derived %s", i, out[i].Address.Hex(), want[i].Hex())
		}
	}
}

// TestBuildUniqueJumpdestInitcodePreAmsterdamShape pins minimum
// invariants on the initcode: non-empty, starts with the seed-JUMPDEST
// PUSH32 prologue, ends with RETURN(0x6000). The EEST-keccak match
// above is the byte-exact contract; this is the structural sanity
// check kept around as fast-fail when the layout shifts.
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

// TestPatternResidentCodeCap pins the I5 hard cap: pattern-mode
// runtimes are byte-unique per derived address and stay resident for
// the whole run (~24.6 KB measured per contract), so a single entity
// above 64 GiB estimated residency is rejected at validate time.
// Production scale (1.5M ≈ 34.3 GiB) must pass. Validation-only — the
// counts here are never expanded.
func TestPatternResidentCodeCap(t *testing.T) {
	if got := CodePatternRuntimeSize(CodePatternUniqueJumpdestPreAmsterdam); got != 24576 {
		t.Fatalf("CodePatternRuntimeSize(pattern) = %d, want 24576", got)
	}
	if got := CodePatternRuntimeSize("bogus"); got != 0 {
		t.Fatalf("CodePatternRuntimeSize(bogus) = %d, want 0", got)
	}
	// floor(64 GiB / 24576) = 2_796_202 fits; +1 exceeds.
	cases := []struct {
		count   uint64
		wantErr bool
	}{
		{1_500_000, false},
		{2_796_202, false},
		{2_796_203, true},
	}
	c2 := &create2DeploysTemplate{}
	cp := &createPreimageDeploysTemplate{}
	for _, c := range cases {
		err := c2.ValidateParameters(map[string]any{
			"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
			"salt_count":   c.count,
		})
		if (err != nil) != c.wantErr {
			t.Errorf("create2_deploys salt_count=%d: err=%v, wantErr=%v", c.count, err, c.wantErr)
		}
		if c.wantErr && err != nil && !strings.Contains(err.Error(), "GiB") {
			t.Errorf("create2_deploys salt_count=%d: error should quote GiB estimate, got %v", c.count, err)
		}
		err = cp.ValidateParameters(map[string]any{
			"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
			"sender":       bittrexAddrHex,
			"count":        c.count,
		})
		if (err != nil) != c.wantErr {
			t.Errorf("create_preimage_deploys count=%d: err=%v, wantErr=%v", c.count, err, c.wantErr)
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
