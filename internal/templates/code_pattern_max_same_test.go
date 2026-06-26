package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestBuildMaxSameRuntimePreAmsterdamLayout(t *testing.T) {
	got := buildMaxSameRuntimePreAmsterdam()

	if len(got) != 0x6000 {
		t.Fatalf("length: got %d, want 0x6000 (24576)", len(got))
	}
	if got[0] != 0x00 {
		t.Errorf("byte[0]: got 0x%02x, want 0x00 (STOP)", got[0])
	}
	for i := 1; i < 0x6000; i++ {
		if got[i] != 0x5B {
			t.Fatalf("byte %d: got 0x%02x, want 0x5B (JUMPDEST filler)", i, got[i])
		}
	}
}

func TestBuildMaxSameInitcodeMatchesEEST(t *testing.T) {
	const eestInitcodeKeccak = "0xe6b00ac4e153de72333bfef8ba8c0241039aa429b54646cf1128e3ca027d7819"
	ic := BuildMaxSameInitcodePreAmsterdam()
	got := crypto.Keccak256Hash(ic).Hex()
	if got != eestInitcodeKeccak {
		t.Fatalf("initcode keccak diverged from EEST: got %s want %s\n"+
			"  state-actor and execution-specs must derive the same CREATE2\n"+
			"  addresses from this initcode; a mismatch means planted\n"+
			"  contracts live at addresses EXISTING_CONTRACT_SAME never queries.",
			got, eestInitcodeKeccak)
	}
}

func TestCreate2DeploysMaxSameAddressesMatchEEST(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternMaxSamePreAmsterdam,
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
		common.HexToAddress("0x457aed6ccdc15f4ee709c7c836168debf8b5fc9d"), // salt 0
		common.HexToAddress("0xcc42664955411919803aa1d5485ccab6584436aa"), // salt 1
	}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("salt=%d: got %s, want EEST-derived %s", i, out[i].Address.Hex(), want[i].Hex())
		}
		if len(out[i].Code) != 1 && out[i].Code[0] != 0x00 {
			t.Errorf("salt=%d: runtime byte[0] = 0x%02x, want STOP", i, out[i].Code[0])
		}
	}
}

func TestMaxSameRuntimeSharedAndExempt(t *testing.T) {
	if got := CodePatternRuntimeSize(CodePatternMaxSamePreAmsterdam); got != 0 {
		t.Fatalf("CodePatternRuntimeSize(max_same) = %d, want 0 (shared/exempt)", got)
	}

	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternMaxSamePreAmsterdam,
		"salt_count":   3,
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// All three derived contracts must point at the same backing array.
	if &out[0].Code[0] != &out[1].Code[0] || &out[1].Code[0] != &out[2].Code[0] {
		t.Errorf("derived runtimes are not aliased: distinct backing arrays")
	}
	// And the bytes must be identical (sanity, not just aliased).
	if !bytes.Equal(out[0].Code, out[1].Code) {
		t.Errorf("derived runtimes differ in bytes")
	}
}
