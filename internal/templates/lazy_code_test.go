package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/spec"
)

// TestPatternCodeIsLazy pins the code streaming contract: byte-unique
// patterns expand to a LazyCode (Code nil) whose bytes match the eager
// builder and the CodeHash set at expansion time; shared-runtime
// patterns keep the shared slice in Code.
func TestPatternCodeIsLazy(t *testing.T) {
	cases := []struct {
		pattern  string
		codeSize uint64
		lazy     bool
	}{
		{CodePatternUniqueJumpdestPreAmsterdam, 0, true},
		{CodePatternMaxDiffPreAmsterdam, 0, true},
		{CodePatternMaxSamePreAmsterdam, 0, false},
		{CodePatternUniqueJumpdest, 0x10000, true},
		{CodePatternMaxDiff, 0x10000, true},
		{CodePatternMaxSame, 0x10000, false},
	}
	for _, c := range cases {
		params := map[string]any{"code_pattern": c.pattern, "salt_count": 3}
		if c.codeSize != 0 {
			params["code_size"] = c.codeSize
		}
		out, err := (&create2DeploysTemplate{}).Expand(Context{}, spec.Entity{
			Kind:       spec.KindContract,
			Template:   TemplateNameCreate2Deploys,
			Parameters: params,
		})
		if err != nil {
			t.Fatalf("%s: Expand: %v", c.pattern, err)
		}
		build, err := codePatternRuntimeBuilder(c.pattern, c.codeSize)
		if err != nil {
			t.Fatalf("%s: codePatternRuntimeBuilder: %v", c.pattern, err)
		}
		for i := range out {
			pe := &out[i]
			if got := pe.LazyCode != nil; got != c.lazy {
				t.Fatalf("%s[%d]: lazy=%v, want %v", c.pattern, i, got, c.lazy)
			}
			if c.lazy && pe.Code != nil {
				t.Errorf("%s[%d]: Code must be nil when LazyCode is set", c.pattern, i)
			}
			code := pe.CodeBytes()
			if want := build(pe.Address); !bytes.Equal(code, want) {
				t.Errorf("%s[%d]: CodeBytes differs from the eager runtime", c.pattern, i)
			}
			if h := crypto.Keccak256Hash(code); !bytes.Equal(h[:], pe.Account.CodeHash) {
				t.Errorf("%s[%d]: CodeHash %x, want keccak(code) %x", c.pattern, i, pe.Account.CodeHash, h)
			}
			if c.lazy && pe.LazyCode.Size != len(code) {
				t.Errorf("%s[%d]: LazyCode.Size %d, want %d", c.pattern, i, pe.LazyCode.Size, len(code))
			}
		}
	}
}

// TestPreimagePatternCodeIsLazy covers the same contract for
// create_preimage_deploys.
func TestPreimagePatternCodeIsLazy(t *testing.T) {
	out, err := (&createPreimageDeploysTemplate{}).Expand(Context{}, spec.Entity{
		Kind:     spec.KindContract,
		Template: TemplateNameCreatePreimageDeploys,
		Parameters: map[string]any{
			"code_pattern": CodePatternMaxDiff,
			"code_size":    0x10000,
			"sender":       bittrexAddrHex,
			"count":        2,
		},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for i := range out {
		pe := &out[i]
		if pe.LazyCode == nil || pe.Code != nil {
			t.Fatalf("[%d]: want LazyCode set and Code nil", i)
		}
		code := pe.CodeBytes()
		if embedded := common.BytesToAddress(code[0x0C:0x20]); embedded != pe.Address {
			t.Errorf("[%d]: embedded address %s, want %s", i, embedded.Hex(), pe.Address.Hex())
		}
		if h := crypto.Keccak256Hash(code); !bytes.Equal(h[:], pe.Account.CodeHash) {
			t.Errorf("[%d]: CodeHash does not match keccak(code)", i)
		}
	}
}
