package templates

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestCREATE2DeploysExpand(t *testing.T) {
	initcode := "0x60016000526001601ff3" // arbitrary 10-byte initcode
	runtime := "0x600160005260206000f3"
	ent := mkContractEntity("create2_deploys", map[string]any{
		"initcode":   initcode,
		"runtime":    runtime,
		"salt_start": 7,
		"salt_count": 3,
	})

	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("count: got %d, want 3", len(out))
	}

	// Recompute expected derivation independently.
	initBytes, _ := ParseHexBytesParam(initcode, "initcode")
	runtimeBytes, _ := ParseHexBytesParam(runtime, "runtime")
	initHash := crypto.Keccak256(initBytes)
	for i, pe := range out {
		var salt [32]byte
		binary.BigEndian.PutUint64(salt[24:], uint64(7+i))
		want := crypto.CreateAddress2(CanonicalCREATE2FactoryAddress, salt, initHash)
		if pe.Address != want {
			t.Errorf("salt=%d addr: got %s, want %s", 7+i, pe.Address.Hex(), want.Hex())
		}
		if string(pe.Code) != string(runtimeBytes) {
			t.Errorf("salt=%d code mismatch", 7+i)
		}
		if pe.Account.Nonce != 1 {
			t.Errorf("salt=%d nonce: got %d, want 1", 7+i, pe.Account.Nonce)
		}
		if pe.Storage != nil {
			t.Errorf("salt=%d: Storage must be nil when storage_init is unset (got non-nil iter)", 7+i)
		}
	}
}

func TestCREATE2DeploysCustomFactory(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"initcode":   "0xfe",
		"runtime":    "0x00",
		"salt_count": 1,
		"factory":    "0x000000000000000000000000000000000000beef",
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("count: %d", len(out))
	}

	// Pin the actual derivation: keccak256(0xff ++ factory ++ salt ++
	// keccak256(initcode))[12:] with the NON-Arachnid factory. Before this
	// assertion existed, an Expand that ignored `factory:` entirely passed
	// this test (only len(out) was checked).
	beef := common.HexToAddress("0x000000000000000000000000000000000000beef")
	initBytes, _ := ParseHexBytesParam("0xfe", "initcode")
	var salt [32]byte // salt_start defaults to 0
	want := crypto.CreateAddress2(beef, salt, crypto.Keccak256(initBytes))
	if out[0].Address != want {
		t.Errorf("derived: got %s, want %s", out[0].Address.Hex(), want.Hex())
	}
	// The custom factory MUST change the derivation — the factory address
	// is a CREATE2 input.
	arachnidDerived := crypto.CreateAddress2(CanonicalCREATE2FactoryAddress, salt, crypto.Keccak256(initBytes))
	if out[0].Address == arachnidDerived {
		t.Errorf("factory parameter ignored: custom-factory derivation equals Arachnid derivation %s",
			arachnidDerived.Hex())
	}
	// Externally derived literal: keccak256(0xff ++ 0x…beef ++ 0^32 ++
	// keccak256(0xfe))[12:], computed twice outside go-ethereum
	// (pycryptodome and eth_hash agree).
	if ext := common.HexToAddress("0x08F2C0A9c0fcB20B1a7Aa2f85F5E0321683d085a"); out[0].Address != ext {
		t.Errorf("derived: got %s, want externally-derived %s", out[0].Address.Hex(), ext.Hex())
	}
}

// TestCREATE2DeploysRejectsZeroSaltCount pins the I3 fix: salt_count=0
// used to expand to zero entities with zero warnings. Both entry points
// now reject it.
func TestCREATE2DeploysRejectsZeroSaltCount(t *testing.T) {
	tmpl := &create2DeploysTemplate{}
	params := map[string]any{
		"initcode":   "0xfe",
		"runtime":    "0x00",
		"salt_count": 0,
	}
	if err := tmpl.ValidateParameters(params); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("ValidateParameters(salt_count=0): want 'must be >= 1' error, got %v", err)
	}
	if _, err := tmpl.Expand(Context{}, mkContractEntity("create2_deploys", params)); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("Expand(salt_count=0): want 'must be >= 1' error, got %v", err)
	}
}

func TestCREATE2DeploysValidate(t *testing.T) {
	tmpl := &create2DeploysTemplate{}
	good := map[string]any{
		"initcode":   "0xfe",
		"runtime":    "0x00",
		"salt_count": 1,
	}
	if err := tmpl.ValidateParameters(good); err != nil {
		t.Errorf("valid: %v", err)
	}
	bad := []map[string]any{
		{"runtime": "0x00", "salt_count": 1},                       // missing initcode
		{"initcode": "0xfe", "runtime": "0x00"},                    // missing salt_count
		{"initcode": "0xfe", "salt_count": 1},                      // missing runtime
		{"initcode": "", "runtime": "0x00", "salt_count": 1},       // empty initcode
		{"initcode": "0xfe", "runtime": "", "salt_count": 1},       // empty runtime
		{"initcode": "0xfe", "runtime": "0x00", "salt_count": "1"}, // bad type
		{"initcode": "0xfe", "runtime": "0x00", "salt_count": 1, "x": 1},
		// `deployed_code` was the old parameter name (renamed to `runtime`);
		// supplying it now must fail loudly so users notice the rename.
		{"initcode": "0xfe", "deployed_code": "0x00", "salt_count": 1},
	}
	for i, p := range bad {
		if err := tmpl.ValidateParameters(p); err == nil {
			t.Errorf("bad[%d]: expected error, got nil for %v", i, p)
		}
	}
}

// TestCREATE2DeploysStorageInit pins that `storage_init` is propagated
// to every derived contract's Storage iter, with slot/value parsed as
// 32-byte hashes. Symmetric with the test in create_preimage_deploys.
func TestCREATE2DeploysStorageInit(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"initcode":   "0xfe",
		"runtime":    "0x00",
		"salt_count": 4,
		"storage_init": map[string]any{
			"0x0": "0xa3c1e324ca1ce40db73ed6026c4a177f099b5770",
			"0x1": "0xff",
		},
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("count: got %d, want 4", len(out))
	}
	wantSlot0 := common.HexToHash("0x000000000000000000000000a3c1e324ca1ce40db73ed6026c4a177f099b5770")
	wantSlot1 := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000000ff")
	for i, pe := range out {
		if pe.Storage == nil {
			t.Fatalf("derived[%d]: Storage must be non-nil when storage_init is set", i)
		}
		pairs := collectPairs(pe.Storage)
		if len(pairs) != 2 {
			t.Fatalf("derived[%d]: storage pair count: got %d, want 2", i, len(pairs))
		}
		gotByKey := make(map[common.Hash]common.Hash, len(pairs))
		for _, p := range pairs {
			gotByKey[p.K] = p.V
		}
		if got := gotByKey[common.Hash{}]; got != wantSlot0 {
			t.Errorf("derived[%d] slot 0: got %s, want %s", i, got.Hex(), wantSlot0.Hex())
		}
		slot1 := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")
		if got := gotByKey[slot1]; got != wantSlot1 {
			t.Errorf("derived[%d] slot 1: got %s, want %s", i, got.Hex(), wantSlot1.Hex())
		}
	}
}
