package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestMaxAdjustableDefaultMatchesPreAmsterdam pins that the size-
// adjustable max_same/max_diff patterns at their 24576 default
// reproduce the pre-Amsterdam builders byte-for-byte.
func TestMaxAdjustableDefaultMatchesPreAmsterdam(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	if !bytes.Equal(BuildMaxSameInitcode(0x6000), BuildMaxSameInitcodePreAmsterdam()) {
		t.Errorf("max_same initcode at code_size=0x6000 differs from pre-Amsterdam builder")
	}
	if !bytes.Equal(BuildMaxDiffInitcode(0x6000), BuildMaxDiffInitcodePreAmsterdam()) {
		t.Errorf("max_diff initcode at code_size=0x6000 differs from pre-Amsterdam builder")
	}
	if !bytes.Equal(BuildMaxDiffRuntime(addr, 0x6000), BuildMaxDiffRuntimePreAmsterdam(addr)) {
		t.Errorf("max_diff runtime at code_size=0x6000 differs from pre-Amsterdam builder")
	}
	if !bytes.Equal(maxSameRuntimeFor(0x6000), maxSameRuntimePreAmsterdam) {
		t.Errorf("max_same runtime at code_size=0x6000 differs from pre-Amsterdam slice")
	}
}

// TestBuildMaxAdjustableInitcodeMatchesEESTSizes pins keccak256 of the
// size-adjustable max_same/max_diff initcode against execution-specs
// `StopJumpdestInitcode(code_size=..., diff=...)`. Regenerate from an
// execution-specs checkout:
//
//	import sys; sys.path.insert(0, 'tests/benchmark')
//	from helper.account_creator import StopJumpdestInitcode
//	from execution_testing import keccak256
//	print(keccak256(bytes(StopJumpdestInitcode(code_size=CS, diff=DIFF))).hex())
//
// If EEST changes the Python initcode shape, update these constants in
// lockstep — both sides must always agree byte-for-byte.
func TestBuildMaxAdjustableInitcodeMatchesEESTSizes(t *testing.T) {
	cases := []struct {
		codeSize uint64
		same     string // diff=False
		diff     string // diff=True
	}{
		{0x20,
			"0x55ff61f1bc93a4eab47a63c38842bac579d0c0b89a1902f16b4c632a77fda2da",
			"0x9ed34dbee4e1d5ef94534f579d7a0dfcefa77eea4218b6734a6b8158c83a7d3b"},
		// Default size: same pins as the *_pre_amsterdam tests.
		{0x6000,
			"0xe6b00ac4e153de72333bfef8ba8c0241039aa429b54646cf1128e3ca027d7819",
			"0xbdaf429840ac400acad3d230653b726a2cdf9201f645976fe353bb45e45bfe63"},
		// EIP-7954 max code size.
		{0x10000,
			"0x4b81bb2f80a8d129fb92d8efac6eeadab97b87b9d2434d2c67929b19a85caff8",
			"0xfb051ac6360cad63b030de61ddcf46c4ee1c00c53cc9d28626c763c9f4763789"},
		{0x20000,
			"0xaf76a7ee78f2422d75e1be552e68ffe99ef5acca33e6d392549f5244fff4da6e",
			"0x70dc09317afb682b90879daa583af3d3f30bcf9bb2cc76a473fc5d144ad014dc"},
		// The size cap shared with unique_jumpdest.
		{0x1000000,
			"0x2c475102e863deb0f7e294692ed287e800937dd70cafae36b44a9a5655515c4f",
			"0x2ef3914ae0c6e0cdc3dda6b961e4d976918b078013de929c790327521279b8e7"},
	}
	for _, c := range cases {
		if got := crypto.Keccak256Hash(BuildMaxSameInitcode(c.codeSize)).Hex(); got != c.same {
			t.Errorf("max_same code_size=%#x: initcode keccak diverged from EEST: got %s want %s",
				c.codeSize, got, c.same)
		}
		if got := crypto.Keccak256Hash(BuildMaxDiffInitcode(c.codeSize)).Hex(); got != c.diff {
			t.Errorf("max_diff code_size=%#x: initcode keccak diverged from EEST: got %s want %s",
				c.codeSize, got, c.diff)
		}
	}
}

// TestCreate2DeploysMaxAdjustableEIP7954AddressesMatchEEST pins derived
// addresses at code_size 0x10000 (EIP-7954 max code size) against
// execution-specs compute_create2_address for both adjustable patterns,
// plus the per-pattern runtime shape.
func TestCreate2DeploysMaxAdjustableEIP7954AddressesMatchEEST(t *testing.T) {
	const codeSize = 0x10000
	cases := []struct {
		pattern string
		want    []common.Address
	}{
		{CodePatternMaxSame, []common.Address{
			common.HexToAddress("0x2563fd1086f400bb9a14b6d81c19a035bec1cdff"), // salt 0
			common.HexToAddress("0xe797e615e99961942ef20c6996a5f6d556652e52"), // salt 1
		}},
		{CodePatternMaxDiff, []common.Address{
			common.HexToAddress("0xb218bb9b64cdd98bdd4e8c727d4ca1b403459e4d"), // salt 0
			common.HexToAddress("0xaa6212db7625db49737957bd9a7179d17239b6af"), // salt 1
		}},
	}
	for _, c := range cases {
		ent := mkContractEntity("create2_deploys", map[string]any{
			"code_pattern": c.pattern,
			"code_size":    codeSize,
			"salt_count":   2,
		})
		out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
		if err != nil {
			t.Fatalf("%s: Expand: %v", c.pattern, err)
		}
		for i := range c.want {
			if out[i].Address != c.want[i] {
				t.Errorf("%s salt=%d: got %s, want EEST-derived %s",
					c.pattern, i, out[i].Address.Hex(), c.want[i].Hex())
			}
			if len(out[i].Code) != codeSize {
				t.Errorf("%s salt=%d: runtime length got %d, want %#x",
					c.pattern, i, len(out[i].Code), codeSize)
			}
			if out[i].Code[0] != 0x00 {
				t.Errorf("%s salt=%d: byte 0 got 0x%02x, want STOP",
					c.pattern, i, out[i].Code[0])
			}
		}
		switch c.pattern {
		case CodePatternMaxSame:
			// Shared runtime: both derived contracts alias identical bytes.
			if !bytes.Equal(out[0].Code, out[1].Code) {
				t.Errorf("max_same: derived runtimes differ (must be byte-identical)")
			}
		case CodePatternMaxDiff:
			for i := range c.want {
				embedded := common.BytesToAddress(out[i].Code[0x0C:0x20])
				if embedded != out[i].Address {
					t.Errorf("max_diff salt=%d: embedded address %s != derived %s",
						i, embedded.Hex(), out[i].Address.Hex())
				}
			}
			if bytes.Equal(out[0].Code, out[1].Code) {
				t.Errorf("max_diff: derived runtimes identical (must be byte-unique)")
			}
		}
	}
}

// TestMaxAdjustableCodeSizeBounds pins the per-pattern minimum: the
// stop-jumpdest layouts accept code_size down to 0x20 (the max_diff
// address region ends there) while unique_jumpdest requires 0x41.
func TestMaxAdjustableCodeSizeBounds(t *testing.T) {
	c2 := &create2DeploysTemplate{}
	for _, pattern := range []string{CodePatternMaxSame, CodePatternMaxDiff} {
		if err := c2.ValidateParameters(map[string]any{
			"code_pattern": pattern, "code_size": 0x20, "salt_count": 1,
		}); err != nil {
			t.Errorf("%s: code_size=0x20 should validate, got %v", pattern, err)
		}
		if err := c2.ValidateParameters(map[string]any{
			"code_pattern": pattern, "code_size": 0x1F, "salt_count": 1,
		}); err == nil {
			t.Errorf("%s: code_size=0x1F should be rejected", pattern)
		}
		if err := c2.ValidateParameters(map[string]any{
			"code_pattern": pattern, "code_size": 0x1000001, "salt_count": 1,
		}); err == nil {
			t.Errorf("%s: code_size above the PUSH3 cap should be rejected", pattern)
		}
	}
	if err := c2.ValidateParameters(map[string]any{
		"code_pattern": CodePatternUniqueJumpdest, "code_size": 0x40, "salt_count": 1,
	}); err == nil {
		t.Errorf("unique_jumpdest: code_size=0x40 should be rejected (min 0x41)")
	}
}

// TestCodePatternRuntimeSizeMaxAdjustable pins residency estimates:
// max_same is shared (exempt, 0); max_diff counts the resolved size,
// defaulting to 24576 when the parameter is omitted.
func TestCodePatternRuntimeSizeMaxAdjustable(t *testing.T) {
	if got := CodePatternRuntimeSize(CodePatternMaxSame, 0x10000); got != 0 {
		t.Errorf("CodePatternRuntimeSize(max_same, 0x10000) = %d, want 0 (shared/exempt)", got)
	}
	if got := CodePatternRuntimeSize(CodePatternMaxDiff, 0x10000); got != 0x10000 {
		t.Errorf("CodePatternRuntimeSize(max_diff, 0x10000) = %d, want 0x10000", got)
	}
	if got := CodePatternRuntimeSize(CodePatternMaxDiff, 0); got != 24576 {
		t.Errorf("CodePatternRuntimeSize(max_diff, 0) = %d, want 24576 (default)", got)
	}
}
