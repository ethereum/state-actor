package templates

import (
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

// TestHonoredEntityFieldsMatrix pins every registered template's
// entity-field declaration against a literal table, so adding a template
// (or changing what one honors) is a deliberate, reviewed decision.
func TestHonoredEntityFieldsMatrix(t *testing.T) {
	type row struct{ balance, nonce, code, approxSize bool }
	want := map[string]row{
		TemplateNameEOA:                       {true, true, true, true},
		TemplateNameRaw:                       {true, true, true, true},
		TemplateNameERC20:                     {true, true, false, true},
		TemplateNameStoragePattern:            {true, true, false, false},
		TemplateNameSequentialEOAs:            {false, false, false, false},
		TemplateNameSequentialPkeyEOAs:        {false, false, false, false},
		TemplateNameSequentialPkeyDelegations: {false, false, false, false},
		TemplateNameCreate2Factory:            {false, false, false, false},
		TemplateNameCreate2Deploys:            {false, false, false, false},
		TemplateNameCreatePreimageDeploys:     {false, false, false, false},
	}
	for _, name := range Names() {
		tmpl, ok := Lookup(name)
		if !ok {
			t.Fatalf("registry lists %q but Lookup fails", name)
		}
		exp, ok := want[name]
		if !ok {
			t.Errorf("template %q has no row in the expected matrix — declare its entity-field support deliberately", name)
			continue
		}
		got := tmpl.HonoredEntityFields()
		if got.Balance.Honored != exp.balance || got.Nonce.Honored != exp.nonce ||
			got.Code.Honored != exp.code || got.ApproximateSizeBytes.Honored != exp.approxSize {
			t.Errorf("%s: declaration {balance:%v nonce:%v code:%v approx:%v}, want {%v %v %v %v}",
				name, got.Balance.Honored, got.Nonce.Honored, got.Code.Honored, got.ApproximateSizeBytes.Honored,
				exp.balance, exp.nonce, exp.code, exp.approxSize)
		}
	}
}

// TestCheckEntityFields pins the checker's semantics: ignored+set →
// error naming the alternative (or "remove it" when none exists);
// honored or zero-valued fields pass.
func TestCheckEntityFields(t *testing.T) {
	seqEOAs, _ := Lookup(TemplateNameSequentialEOAs)
	factory, _ := Lookup(TemplateNameCreate2Factory)
	eoa, _ := Lookup(TemplateNameEOA)

	bal := spec.BigIntDecimal{V: uint256.NewInt(1)}
	zeroBal := spec.BigIntDecimal{V: uint256.NewInt(0)}

	// Ignored field with an alternative → error suggesting it.
	err := CheckEntityFields(seqEOAs, spec.Entity{Balance: &bal})
	if err == nil || !strings.Contains(err.Error(), "entity-level `balance:` is ignored") ||
		!strings.Contains(err.Error(), "parameters.balance") {
		t.Errorf("balance on sequential_eoas: want ignored+alternative error, got %v", err)
	}

	// Explicit balance "0" is still a set pointer → rejected.
	if err := CheckEntityFields(seqEOAs, spec.Entity{Balance: &zeroBal}); err == nil {
		t.Errorf("explicit balance \"0\" on sequential_eoas: want error, got nil")
	}

	// Ignored field with no alternative → "remove it".
	err = CheckEntityFields(factory, spec.Entity{Nonce: 7})
	if err == nil || !strings.Contains(err.Error(), "remove it") {
		t.Errorf("nonce on create2_factory: want 'remove it' error, got %v", err)
	}

	// nonce: 0 is indistinguishable from unset — never flagged.
	if err := CheckEntityFields(seqEOAs, spec.Entity{Nonce: 0}); err != nil {
		t.Errorf("nonce=0 must not be flagged: %v", err)
	}

	// Fully-honoring template accepts everything.
	full := spec.Entity{
		Balance:              &bal,
		Nonce:                3,
		Code:                 []byte{0x60},
		ApproximateSizeBytes: 1024,
	}
	if err := CheckEntityFields(eoa, full); err != nil {
		t.Errorf("eoa honors all fields; got %v", err)
	}
}
