package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestBuildMaxDiffRuntimePreAmsterdamLayout pins the per-address runtime
// against execution-specs UniqueMaxContractInitcode(diff=True): STOP +
// 11 zero padding bytes, the 20-byte address at 0x0C..0x20, JUMPDEST sea.
func TestBuildMaxDiffRuntimePreAmsterdamLayout(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	got := BuildMaxDiffRuntimePreAmsterdam(addr)

	if len(got) != 0x6000 {
		t.Fatalf("length: got %d, want 0x6000 (24576)", len(got))
	}
	// Bytes 0x00..0x0C: STOP at 0 plus 11 zero padding bytes.
	for i := 0; i < 0x0C; i++ {
		if got[i] != 0x00 {
			t.Fatalf("byte %d: got 0x%02x, want 0x00", i, got[i])
		}
	}
	// Bytes 0x0C..0x20: embedded address.
	if !bytes.Equal(got[0x0C:0x20], addr[:]) {
		t.Errorf("embedded address [0x0C:0x20]: got %x, want %x", got[0x0C:0x20], addr[:])
	}
	// Bytes 0x20..0x6000: JUMPDEST filler.
	for i := 0x20; i < 0x6000; i++ {
		if got[i] != 0x5B {
			t.Fatalf("byte %d: got 0x%02x, want 0x5B (JUMPDEST filler)", i, got[i])
		}
	}
}

// TestBuildMaxDiffRuntimeUnique pins that each derived address produces a
// byte-distinct runtime — they differ only in the embedded-address region.
func TestBuildMaxDiffRuntimeUnique(t *testing.T) {
	a := BuildMaxDiffRuntimePreAmsterdam(common.HexToAddress("0x01"))
	b := BuildMaxDiffRuntimePreAmsterdam(common.HexToAddress("0x02"))
	if bytes.Equal(a, b) {
		t.Fatalf("two distinct addresses produced byte-identical runtime")
	}
	if !bytes.Equal(a[:0x0C], b[:0x0C]) {
		t.Errorf("prefix [0:0x0C] should be identical; differ")
	}
	if !bytes.Equal(a[0x20:], b[0x20:]) {
		t.Errorf("suffix [0x20:] should be identical; differ")
	}
}

// TestBuildMaxDiffInitcodeMatchesEEST pins the keccak256 of the
// state-actor-emitted initcode to the value EEST's
// UniqueMaxContractInitcode(diff=True) produces, so state-actor and
// execution-specs derive the same CREATE2 addresses for
// AccountMode.EXISTING_CONTRACT_DIFF.
//
// Regenerate from an execution-specs checkout:
//
//	from tests.benchmark.helper.account_creator import (
//	    UniqueMaxContractInitcode)
//	from eth_utils import keccak
//	print(keccak(bytes(UniqueMaxContractInitcode(diff=True))).hex())
func TestBuildMaxDiffInitcodeMatchesEEST(t *testing.T) {
	const eestInitcodeKeccak = "0xbdaf429840ac400acad3d230653b726a2cdf9201f645976fe353bb45e45bfe63"
	ic := BuildMaxDiffInitcodePreAmsterdam()
	got := crypto.Keccak256Hash(ic).Hex()
	if got != eestInitcodeKeccak {
		t.Fatalf("initcode keccak diverged from EEST: got %s want %s\n"+
			"  a mismatch means planted contracts live at addresses\n"+
			"  EXISTING_CONTRACT_DIFF never queries.",
			got, eestInitcodeKeccak)
	}
}

// TestCreate2DeploysMaxDiffAddressesMatchEEST pins two DERIVED ADDRESSES
// (canonical Arachnid factory, salts 0 and 1, initcode = max_diff
// pattern) to literals computed outside go-ethereum (eth_utils keccak):
//
//	keccak256(0xff ++ 4e59…956c ++ uint256(salt) ++
//	          bdaf429840ac400acad3d230653b726a2cdf9201f645976fe353bb45e45bfe63)[12:]
func TestCreate2DeploysMaxDiffAddressesMatchEEST(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternMaxDiffPreAmsterdam,
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
		common.HexToAddress("0xa9cb82ea3c688e8c8153e60c1e340840459f66a6"), // salt 0
		common.HexToAddress("0x99ea5fb3758b5fa20b4dfbd2971e98e9723f8f1f"), // salt 1
	}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("salt=%d: got %s, want EEST-derived %s", i, out[i].Address.Hex(), want[i].Hex())
		}
		// The derived contract must embed its own address in the runtime.
		if !bytes.Equal(out[i].Code[0x0C:0x20], out[i].Address[:]) {
			t.Errorf("salt=%d: runtime does not embed derived address", i)
		}
	}
}

// TestMaxDiffRuntimeSizeCounted pins that max-diff is treated as a
// byte-unique pattern for residency accounting (unlike max-same).
func TestMaxDiffRuntimeSizeCounted(t *testing.T) {
	if got := CodePatternRuntimeSize(CodePatternMaxDiffPreAmsterdam); got != 0x6000 {
		t.Fatalf("CodePatternRuntimeSize(max_diff) = %d, want 24576 (unique/counted)", got)
	}
}
