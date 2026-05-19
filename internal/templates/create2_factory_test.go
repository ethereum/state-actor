package templates

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
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

func TestCREATE2FactoryWrongAddress(t *testing.T) {
	ent := mkContractEntity("create2_factory", nil)
	wrong := common.HexToAddress("0x0000000000000000000000000000000000000001")
	_, err := (&create2FactoryTemplate{}).Expand(Context{ResolvedAddress: wrong}, ent)
	if err == nil {
		t.Fatalf("expected error when address != canonical factory")
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
