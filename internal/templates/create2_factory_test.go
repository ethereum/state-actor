package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nerolation/state-actor/internal/spec"
)

func TestCREATE2FactoryExpand(t *testing.T) {
	ent := mkContractEntity("create2_factory", nil)
	ctx := Context{ResolvedAddress: CanonicalCREATE2FactoryAddress}
	out, err := (&create2FactoryTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 PreAllocEntity, got %d", len(out))
	}
	pe := out[0]
	if pe.Address != CanonicalCREATE2FactoryAddress {
		t.Errorf("address: got %s, want %s", pe.Address.Hex(), CanonicalCREATE2FactoryAddress.Hex())
	}
	if !bytes.Equal(pe.Code, CanonicalCREATE2FactoryCode) {
		t.Errorf("Code mismatch with canonical factory runtime")
	}
	want := crypto.Keccak256Hash(CanonicalCREATE2FactoryCode).Bytes()
	if !bytes.Equal(pe.Account.CodeHash, want) {
		t.Errorf("CodeHash mismatch")
	}
	if pe.Account.Nonce != 1 {
		t.Errorf("nonce: got %d, want 1", pe.Account.Nonce)
	}
}

// TestCREATE2FactoryDefaultsToArachnid pins the new default: when the
// user supplies neither `address:` nor `name:`, the template plants the
// factory runtime at the canonical Arachnid address regardless of what
// ResolveAddress hands back via ctx.ResolvedAddress.
func TestCREATE2FactoryDefaultsToArachnid(t *testing.T) {
	ent := mkContractEntity("create2_factory", nil) // no Address, no Name
	// Simulate the position-derived address ResolveAddress would compute.
	positional := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	out, err := (&create2FactoryTemplate{}).Expand(
		Context{ResolvedAddress: positional}, ent,
	)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 || out[0].Address != CanonicalCREATE2FactoryAddress {
		t.Errorf("default address: got %s, want %s",
			out[0].Address.Hex(), CanonicalCREATE2FactoryAddress.Hex())
	}
}

// TestCREATE2FactoryExplicitNonArachnidAddress pins that explicit
// `address:` (non-Arachnid) is honored — the template plants the
// canonical Arachnid runtime at the user-chosen address.
func TestCREATE2FactoryExplicitNonArachnidAddress(t *testing.T) {
	custom := common.HexToAddress("0x000000000000000000000000000000000000beef")
	ent := mkContractEntity("create2_factory", nil)
	addr := spec.HexAddress(custom)
	ent.Address = &addr
	out, err := (&create2FactoryTemplate{}).Expand(
		Context{ResolvedAddress: custom}, ent,
	)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("count: got %d, want 1", len(out))
	}
	if out[0].Address != custom {
		t.Errorf("address: got %s, want %s (explicit)", out[0].Address.Hex(), custom.Hex())
	}
	if !bytes.Equal(out[0].Code, CanonicalCREATE2FactoryCode) {
		t.Errorf("Code should still be the canonical Arachnid runtime regardless of address")
	}
}

// TestCREATE2FactoryNameDerivedAddress pins the name-derived path: when
// `name:` is set but `address:` isn't, the template uses
// ctx.ResolvedAddress as-is (no Arachnid override). The user opted in to
// the name-derived address by setting `name:` explicitly.
func TestCREATE2FactoryNameDerivedAddress(t *testing.T) {
	derived := common.HexToAddress("0xcafe00000000000000000000000000000000beef")
	ent := mkContractEntity("create2_factory", nil)
	ent.Name = "named-factory"
	out, err := (&create2FactoryTemplate{}).Expand(
		Context{ResolvedAddress: derived}, ent,
	)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if out[0].Address != derived {
		t.Errorf("name-derived address: got %s, want %s", out[0].Address.Hex(), derived.Hex())
	}
}

func TestCREATE2FactoryValidate(t *testing.T) {
	tmpl := &create2FactoryTemplate{}
	if err := tmpl.ValidateParameters(nil); err != nil {
		t.Errorf("nil params: %v", err)
	}
	if err := tmpl.ValidateParameters(map[string]any{}); err != nil {
		t.Errorf("empty params: %v", err)
	}
	if err := tmpl.ValidateParameters(map[string]any{"x": 1}); err == nil {
		t.Errorf("any param: expected error")
	}
}
