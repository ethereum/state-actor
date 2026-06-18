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

// TestEffectiveCreate2FactoryAddress pins the shared defaulting rule both
// create2FactoryTemplate.Expand and specbuild's Arachnid-pairing
// enforcement delegate to: neither `address:` nor `name:` → canonical
// Arachnid; anything explicit → the resolved address verbatim.
func TestEffectiveCreate2FactoryAddress(t *testing.T) {
	resolved := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	if got := EffectiveCreate2FactoryAddress(spec.Entity{}, resolved); got != CanonicalCREATE2FactoryAddress {
		t.Errorf("defaulted: got %s, want canonical %s", got.Hex(), CanonicalCREATE2FactoryAddress.Hex())
	}

	custom := spec.HexAddress(common.HexToAddress("0x000000000000000000000000000000000000beef"))
	if got := EffectiveCreate2FactoryAddress(spec.Entity{Address: &custom}, resolved); got != resolved {
		t.Errorf("explicit address: got %s, want resolved %s", got.Hex(), resolved.Hex())
	}

	if got := EffectiveCreate2FactoryAddress(spec.Entity{Name: "named-factory"}, resolved); got != resolved {
		t.Errorf("named: got %s, want resolved %s", got.Hex(), resolved.Hex())
	}
}

// TestCanonicalCREATE2FactoryCodeKeccakPin pins CanonicalCREATE2FactoryCode
// to the keccak256 of the on-chain Arachnid deterministic-deployment proxy
// runtime, so an accidental edit to the vendored constant cannot slip by:
// every other test compares the constant only against itself, and a broken
// factory body means run-time CREATE2 deploy transactions call into garbage.
//
// The pinned hash was derived OUTSIDE go-ethereum, with two independent
// keccak implementations that agree:
//
//	python3 -c "from Crypto.Hash import keccak; print('0x'+keccak.new(
//	    digest_bits=256, data=bytes.fromhex('<runtime hex>')).hexdigest())"
//	python3 -c "from eth_hash.auto import keccak; print('0x'+keccak(
//	    bytes.fromhex('<runtime hex>')).hex())"
//
// and can be cross-checked against any chain (the same bytes live at the
// same address everywhere):
//
//	cast code 0x4e59b44847b379578588920cA78FbF26c0B4956C --rpc-url <mainnet>
func TestCanonicalCREATE2FactoryCodeKeccakPin(t *testing.T) {
	const wantLen = 69
	const wantKeccak = "0x2fa86add0aed31f33a762c9d88e807c475bd51d0f52bd0955754b2608f7e4989"
	if len(CanonicalCREATE2FactoryCode) != wantLen {
		t.Fatalf("length: got %d, want %d", len(CanonicalCREATE2FactoryCode), wantLen)
	}
	if got := crypto.Keccak256Hash(CanonicalCREATE2FactoryCode).Hex(); got != wantKeccak {
		t.Fatalf("keccak256(CanonicalCREATE2FactoryCode) = %s, want %s (vendored Arachnid runtime was modified?)",
			got, wantKeccak)
	}
}
