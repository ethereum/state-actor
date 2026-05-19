package templates

import (
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestCREATE2DeploysExpand(t *testing.T) {
	initcode := "0x60016000526001601ff3" // arbitrary 10-byte initcode
	deployedCode := "0x600160005260206000f3"
	ent := mkContractEntity("create2_deploys", map[string]any{
		"initcode":      initcode,
		"deployed_code": deployedCode,
		"salt_start":    7,
		"salt_count":    3,
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
	dcodeBytes, _ := ParseHexBytesParam(deployedCode, "deployed_code")
	initHash := crypto.Keccak256(initBytes)
	for i, pe := range out {
		var salt [32]byte
		binary.BigEndian.PutUint64(salt[24:], uint64(7+i))
		want := crypto.CreateAddress2(CanonicalCREATE2FactoryAddress, salt, initHash)
		if pe.Address != want {
			t.Errorf("salt=%d addr: got %s, want %s", 7+i, pe.Address.Hex(), want.Hex())
		}
		if string(pe.Code) != string(dcodeBytes) {
			t.Errorf("salt=%d code mismatch", 7+i)
		}
		if pe.Account.Nonce != 1 {
			t.Errorf("salt=%d nonce: got %d, want 1", 7+i, pe.Account.Nonce)
		}
	}
}

func TestCREATE2DeploysCustomFactory(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"initcode":      "0xfe",
		"deployed_code": "0x00",
		"salt_count":    1,
		"factory":       "0x000000000000000000000000000000000000beef",
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("count: %d", len(out))
	}
}

func TestCREATE2DeploysZeroSaltCount(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"initcode":      "0xfe",
		"deployed_code": "0x00",
		"salt_count":    0,
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("salt_count=0 must emit nothing, got %d", len(out))
	}
}

func TestCREATE2DeploysValidate(t *testing.T) {
	tmpl := &create2DeploysTemplate{}
	good := map[string]any{
		"initcode":      "0xfe",
		"deployed_code": "0x00",
		"salt_count":    1,
	}
	if err := tmpl.ValidateParameters(good); err != nil {
		t.Errorf("valid: %v", err)
	}
	bad := []map[string]any{
		{"deployed_code": "0x00", "salt_count": 1},                       // missing initcode
		{"initcode": "0xfe", "deployed_code": "0x00"},                    // missing salt_count
		{"initcode": "0xfe", "salt_count": 1},                            // missing deployed_code
		{"initcode": "", "deployed_code": "0x00", "salt_count": 1},       // empty initcode
		{"initcode": "0xfe", "deployed_code": "", "salt_count": 1},       // empty deployed_code
		{"initcode": "0xfe", "deployed_code": "0x00", "salt_count": "1"}, // bad type
		{"initcode": "0xfe", "deployed_code": "0x00", "salt_count": 1, "x": 1},
	}
	for i, p := range bad {
		if err := tmpl.ValidateParameters(p); err == nil {
			t.Errorf("bad[%d]: expected error, got nil for %v", i, p)
		}
	}
}
