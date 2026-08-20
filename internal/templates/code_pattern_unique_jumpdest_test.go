package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestUniqueJumpdestDefaultMatchesPreAmsterdam pins that the
// size-adjustable pattern at its 24576 default reproduces the
// pre-Amsterdam builders byte-for-byte — the property that lets state
// already deployed on live testnets keep its CREATE2 addresses.
func TestUniqueJumpdestDefaultMatchesPreAmsterdam(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	if !bytes.Equal(
		BuildUniqueJumpdestRuntime(addr, 0x6000),
		BuildUniqueJumpdestRuntimePreAmsterdam(addr),
	) {
		t.Errorf("runtime at code_size=0x6000 differs from pre-Amsterdam builder")
	}
	if !bytes.Equal(
		BuildUniqueJumpdestInitcode(0x6000),
		BuildUniqueJumpdestInitcodePreAmsterdam(),
	) {
		t.Errorf("initcode at code_size=0x6000 differs from pre-Amsterdam builder")
	}
}

// TestBuildUniqueJumpdestInitcodeMatchesEESTSizes pins keccak256 of the
// size-adjustable initcode against execution-specs
// `JochemnetPredeployContractInitcode(code_size=...)` for sizes at every
// entry-push width and on both sides of each boundary (0x100 and
// 0x010000). Regenerate from an execution-specs checkout:
//
//	import sys; sys.path.insert(0, 'tests/benchmark')
//	from helper.account_creator import JochemnetPredeployContractInitcode
//	from execution_testing import keccak256
//	print(keccak256(bytes(JochemnetPredeployContractInitcode(code_size=CS))).hex())
//
// If EEST changes the Python initcode shape, update these constants in
// lockstep — both sides must always agree byte-for-byte.
func TestBuildUniqueJumpdestInitcodeMatchesEESTSizes(t *testing.T) {
	cases := []struct {
		codeSize uint64
		keccak   string
	}{
		// Minimum size, and the largest still entered by a PUSH1.
		{0x41, "0xa04269b63348a0db52e897141f3133c359dc29ab105f3486511f7f9372159134"},
		{0x100, "0x068a051642eef559a9da27724afa363d316e26b3d4f473d5c556b6ba74d347c4"},
		// First PUSH2-encoded size (entry `PUSH2 0x0100; JUMP`).
		{0x101, "0x9d85953b1bfac9014d0b7bb053c16f46d4653d7f9d1cbc6c6cd6459a0f98e889"},
		// Default size: same pin as TestBuildUniqueJumpdestInitcodeMatchesEEST.
		{0x6000, "0xb9cdb9047474294c9743cf3944156c844bf91763de66271493caa07a3de77ec5"},
		// Largest PUSH2-encoded size (entry `PUSH2 0xFFFF; JUMP`).
		{0x10000, "0xc255e15a2e24a89fd585858f4b20b46fe8eab3ce309d994be18f343f539823bb"},
		// First PUSH3-encoded size (entry `PUSH3 0x010000; JUMP`).
		{0x10001, "0xfb6c8fdbd5b4f1624cb84a9fc37181e6aab74f3bbb0990f58e6d13c49cd8baf8"},
		{0x20000, "0xc03348b4fde3c0eed2f9707c8b003c62ae4752d85f36544d1a2ddad623114d07"},
		// The capped maximum.
		{0x1000000, "0x51f4a0dab520358d8b1a8c641f318e3fefbf722c164807c11c1ee76504ffc5d2"},
	}
	for _, c := range cases {
		got := crypto.Keccak256Hash(BuildUniqueJumpdestInitcode(c.codeSize)).Hex()
		if got != c.keccak {
			t.Errorf("code_size=%#x: initcode keccak diverged from EEST: got %s want %s",
				c.codeSize, got, c.keccak)
		}
	}
}

// TestBuildUniqueJumpdestRuntimeLayoutPush3 pins the runtime layout for
// a size above the PUSH2→PUSH3 boundary: 5-byte entry, JUMPDEST padding
// through 0x2C, embedded address, JUMPDEST filler to the end.
func TestBuildUniqueJumpdestRuntimeLayoutPush3(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	const codeSize = 0x20000
	got := BuildUniqueJumpdestRuntime(addr, codeSize)

	if len(got) != codeSize {
		t.Fatalf("length: got %d, want %#x", len(got), codeSize)
	}
	// Entry: PUSH3 0x01FFFF; JUMP.
	want := []byte{0x62, 0x01, 0xFF, 0xFF, 0x56}
	if !bytes.Equal(got[0:5], want) {
		t.Errorf("entry bytes [0:5]: got %x, want %x", got[0:5], want)
	}
	// Bytes 0x05..0x2C: JUMPDEST padding.
	for i := 0x05; i < 0x2C; i++ {
		if got[i] != 0x5B {
			t.Fatalf("byte %d: got 0x%02x, want 0x5B (JUMPDEST padding)", i, got[i])
		}
	}
	// Bytes 0x2C..0x40: embedded address.
	if !bytes.Equal(got[0x2C:0x40], addr[:]) {
		t.Errorf("embedded address [0x2C:0x40]: got %x, want %x", got[0x2C:0x40], addr[:])
	}
	// All remaining bytes (including the JUMP target at codeSize-1) must
	// be JUMPDEST.
	for i := 0x40; i < codeSize; i++ {
		if got[i] != 0x5B {
			t.Fatalf("byte %d: got 0x%02x, want 0x5B (JUMPDEST filler)", i, got[i])
		}
	}
}

// TestBuildUniqueJumpdestRuntimeEntryBoundary pins the entry encoding at
// each width the minimal push produces, including both sides of the two
// boundaries. Every value here is `bytes(Op.JUMP(code_size - 1))` read
// out of execution-specs, which is the point: the entry was a
// fixed-width PUSH2 before and disagreed with EEST for every size whose
// target fits one byte.
func TestBuildUniqueJumpdestRuntimeEntryBoundary(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	cases := []struct {
		codeSize uint64
		entry    []byte
	}{
		{0x41, []byte{0x60, 0x40, 0x56}},                // PUSH1, the minimum size
		{0x100, []byte{0x60, 0xFF, 0x56}},               // largest PUSH1
		{0x101, []byte{0x61, 0x01, 0x00, 0x56}},         // first PUSH2
		{0x6000, []byte{0x61, 0x5F, 0xFF, 0x56}},        // the default
		{0x10000, []byte{0x61, 0xFF, 0xFF, 0x56}},       // largest PUSH2
		{0x10001, []byte{0x62, 0x01, 0x00, 0x00, 0x56}}, // first PUSH3
		{0x1000000, []byte{0x62, 0xFF, 0xFF, 0xFF, 0x56}},
	}
	for _, c := range cases {
		got := BuildUniqueJumpdestRuntime(addr, c.codeSize)
		if !bytes.Equal(got[:len(c.entry)], c.entry) {
			t.Errorf("code_size=%#x: entry got %x, want %x",
				c.codeSize, got[:len(c.entry)], c.entry)
		}
		if got[len(c.entry)] != 0x5B {
			t.Errorf("code_size=%#x: byte after entry got 0x%02x, want 0x5B",
				c.codeSize, got[len(c.entry)])
		}
	}
}

// TestCreate2DeploysUniqueJumpdestCodeSizeAddressesMatchEEST pins two
// derived addresses for a PUSH3-range code_size against values computed
// by execution-specs. Regenerate:
//
//	import sys; sys.path.insert(0, 'tests/benchmark')
//	from helper.account_creator import JochemnetPredeployContractInitcode
//	from execution_testing import compute_create2_address
//	ic = bytes(JochemnetPredeployContractInitcode(code_size=0x20000))
//	print(compute_create2_address(0x4e59b44847b379578588920cA78FbF26c0B4956C, 0, ic))
func TestCreate2DeploysUniqueJumpdestCodeSizeAddressesMatchEEST(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternUniqueJumpdest,
		"code_size":    0x20000,
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
		common.HexToAddress("0xdf68ff7c58c2965a48b40a7d2aaab61932191c46"), // salt 0
		common.HexToAddress("0x57fab4217b2eddcf813fd9956ff07872ef53d067"), // salt 1
	}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("salt=%d: got %s, want EEST-derived %s", i, out[i].Address.Hex(), want[i].Hex())
		}
		if len(out[i].Code) != 0x20000 {
			t.Errorf("salt=%d: runtime length got %d, want 0x20000", i, len(out[i].Code))
		}
		embedded := common.BytesToAddress(out[i].Code[0x2C:0x40])
		if embedded != out[i].Address {
			t.Errorf("salt=%d: embedded address %s != derived %s",
				i, embedded.Hex(), out[i].Address.Hex())
		}
	}
}

// TestCreate2DeploysUniqueJumpdestDefaultSizeAddresses pins that the
// size-adjustable pattern WITHOUT code_size derives the exact same
// addresses as the fixed pre-Amsterdam pattern (identical initcode at
// the shared 24576 default).
func TestCreate2DeploysUniqueJumpdestDefaultSizeAddresses(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternUniqueJumpdest,
		"salt_count":   2,
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []common.Address{
		common.HexToAddress("0xEC9B57721DeaF87D94C5aB94E3c919c60b42aAd8"), // salt 0
		common.HexToAddress("0x59B3D48aEA1b74c1BD4850f471aD067A8c3bc5Aa"), // salt 1
	}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("salt=%d: got %s, want %s (must equal the pre-Amsterdam pattern's address)",
				i, out[i].Address.Hex(), want[i].Hex())
		}
	}
}

// TestCodeSizeParameterValidation pins the code_size parameter rules:
// only valid together with a size-adjustable code_pattern, and only
// within [0x41, 0x01000000].
func TestCodeSizeParameterValidation(t *testing.T) {
	c2 := &create2DeploysTemplate{}
	cp := &createPreimageDeploysTemplate{}
	pk := &sequentialPkeyDelegationsTemplate{}

	rejected := []struct {
		name   string
		tmpl   interface{ ValidateParameters(map[string]any) error }
		params map[string]any
	}{
		{"create2: code_size with fixed-size pattern", c2, map[string]any{
			"code_pattern": CodePatternUniqueJumpdestPreAmsterdam,
			"code_size":    0x20000,
			"salt_count":   1,
		}},
		{"create2: code_size without code_pattern", c2, map[string]any{
			"initcode":   "0xfe",
			"runtime":    "0xfe",
			"code_size":  0x20000,
			"salt_count": 1,
		}},
		{"create2: code_size below minimum", c2, map[string]any{
			"code_pattern": CodePatternUniqueJumpdest,
			"code_size":    0x40,
			"salt_count":   1,
		}},
		{"create2: code_size above cap", c2, map[string]any{
			"code_pattern": CodePatternUniqueJumpdest,
			"code_size":    0x1000001,
			"salt_count":   1,
		}},
		{"preimage: code_size with fixed-size pattern", cp, map[string]any{
			"code_pattern": CodePatternMaxDiffPreAmsterdam,
			"code_size":    0x20000,
			"sender":       bittrexAddrHex,
			"count":        1,
		}},
		{"preimage: code_size without code_pattern", cp, map[string]any{
			"runtime":   "0xfe",
			"code_size": 0x20000,
			"sender":    bittrexAddrHex,
			"count":     1,
		}},
		{"pkey_delegations: code_size without code_pattern", pk, map[string]any{
			"start_pkey": "0x0000000000000000000000000000000000000000000000000000000000000001",
			"count":      1,
			"initcode":   "0xfe",
			"code_size":  0x20000,
		}},
		{"pkey_delegations: code_size with fixed-size pattern", pk, map[string]any{
			"start_pkey":   "0x0000000000000000000000000000000000000000000000000000000000000001",
			"count":        1,
			"code_pattern": CodePatternMaxSamePreAmsterdam,
			"code_size":    0x20000,
		}},
	}
	for _, c := range rejected {
		if err := c.tmpl.ValidateParameters(c.params); err == nil {
			t.Errorf("%s: expected validation error, got nil", c.name)
		}
	}

	accepted := []struct {
		name   string
		tmpl   interface{ ValidateParameters(map[string]any) error }
		params map[string]any
	}{
		{"create2: minimum code_size", c2, map[string]any{
			"code_pattern": CodePatternUniqueJumpdest,
			"code_size":    0x41,
			"salt_count":   1,
		}},
		{"create2: maximum code_size", c2, map[string]any{
			"code_pattern": CodePatternUniqueJumpdest,
			"code_size":    0x1000000,
			"salt_count":   1,
		}},
		{"preimage: adjustable pattern with code_size", cp, map[string]any{
			"code_pattern": CodePatternUniqueJumpdest,
			"code_size":    0x20000,
			"sender":       bittrexAddrHex,
			"count":        1,
		}},
		{"pkey_delegations: adjustable pattern with code_size", pk, map[string]any{
			"start_pkey":   "0x0000000000000000000000000000000000000000000000000000000000000001",
			"count":        1,
			"code_pattern": CodePatternUniqueJumpdest,
			"code_size":    0x20000,
		}},
	}
	for _, c := range accepted {
		if err := c.tmpl.ValidateParameters(c.params); err != nil {
			t.Errorf("%s: unexpected validation error: %v", c.name, err)
		}
	}
}

// TestCodePatternRuntimeSizeUniqueJumpdest pins the residency estimate
// for the size-adjustable pattern: the resolved code_size when set, the
// 24576 default when the parameter is omitted (codeSize=0).
func TestCodePatternRuntimeSizeUniqueJumpdest(t *testing.T) {
	if got := CodePatternRuntimeSize(CodePatternUniqueJumpdest, 0); got != 24576 {
		t.Errorf("CodePatternRuntimeSize(unique_jumpdest, 0) = %d, want 24576", got)
	}
	if got := CodePatternRuntimeSize(CodePatternUniqueJumpdest, 0x20000); got != 0x20000 {
		t.Errorf("CodePatternRuntimeSize(unique_jumpdest, 0x20000) = %d, want 0x20000", got)
	}
}

// TestPatternResidentCodeCapLargeCodeSize pins the 64 GiB residency cap
// against the 16 MiB maximum code_size: 4096 × 16 MiB = 64 GiB exactly
// (allowed, cap is exclusive), 4097 exceeds.
func TestPatternResidentCodeCapLargeCodeSize(t *testing.T) {
	c2 := &create2DeploysTemplate{}
	base := map[string]any{
		"code_pattern": CodePatternUniqueJumpdest,
		"code_size":    0x1000000,
	}
	ok := map[string]any{"salt_count": 4096}
	bad := map[string]any{"salt_count": 4097}
	for k, v := range base {
		ok[k] = v
		bad[k] = v
	}
	if err := c2.ValidateParameters(ok); err != nil {
		t.Errorf("salt_count=4096 at 16 MiB should pass the 64 GiB cap, got %v", err)
	}
	if err := c2.ValidateParameters(bad); err == nil {
		t.Errorf("salt_count=4097 at 16 MiB should exceed the 64 GiB cap, got nil")
	}
}
