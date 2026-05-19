package templates

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestStoragePatternExpand(t *testing.T) {
	addr := common.HexToAddress("0x3f8074692982594c1936bd27433a8b6e5d77e0f0")
	ent := mkContractEntity("storage_pattern", map[string]any{"final": 4})
	ctx := Context{ResolvedAddress: addr}

	out, err := (&storagePatternTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 PreAllocEntity, got %d", len(out))
	}
	pe := out[0]
	if pe.Address != addr {
		t.Errorf("address: got %s, want %s", pe.Address.Hex(), addr.Hex())
	}
	if pe.Account.Nonce != 1 {
		t.Errorf("nonce: got %d, want >= 1 (defaults to 1)", pe.Account.Nonce)
	}
	if pe.Code != nil {
		t.Errorf("Code must be nil (storage_pattern leaves the address codeless)")
	}
	if string(pe.Account.CodeHash) != string(types.EmptyCodeHash[:]) {
		t.Errorf("CodeHash must be EmptyCodeHash")
	}

	pairs := collectPairs(pe.Storage)
	if len(pairs) != 5 { // slot 0 + slots 1..4
		t.Fatalf("slot count: got %d, want 5", len(pairs))
	}
	// slot 0 -> 5 (final+1)
	if pairs[0].K != (common.Hash{}) || pairs[0].V != uint64ToHash(5) {
		t.Errorf("slot 0: got (%x, %x), want (0, 5)", pairs[0].K, pairs[0].V)
	}
	for i := 1; i <= 4; i++ {
		want := uint64ToHash(uint64(i))
		if pairs[i].K != want || pairs[i].V != want {
			t.Errorf("slot %d: got (%x, %x), want (%x, %x)", i, pairs[i].K, pairs[i].V, want, want)
		}
	}
}

func TestStoragePatternZeroFinal(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	ent := mkContractEntity("storage_pattern", map[string]any{"final": 0})
	out, err := (&storagePatternTemplate{}).Expand(Context{ResolvedAddress: addr}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	pairs := collectPairs(out[0].Storage)
	// Only slot 0 = 1 (final+1).
	if len(pairs) != 1 || pairs[0].K != (common.Hash{}) || pairs[0].V != uint64ToHash(1) {
		t.Errorf("final=0: got pairs=%v, want [{slot=0, val=1}]", pairs)
	}
}

func TestStoragePatternIterReplayable(t *testing.T) {
	// The PreAllocEntity.Storage iterator must be safe to consume more
	// than once (writers may iterate twice — count vs. emit).
	addr := common.HexToAddress("0x0000000000000000000000000000000000000002")
	ent := mkContractEntity("storage_pattern", map[string]any{"final": 100})
	out, _ := (&storagePatternTemplate{}).Expand(Context{ResolvedAddress: addr}, ent)
	a := collectPairs(out[0].Storage)
	b := collectPairs(out[0].Storage)
	if len(a) != len(b) {
		t.Fatalf("iter not replayable: len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("replay mismatch at %d: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestStoragePatternValidate(t *testing.T) {
	tmpl := &storagePatternTemplate{}
	if err := tmpl.ValidateParameters(map[string]any{"final": 100}); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := tmpl.ValidateParameters(map[string]any{}); err == nil {
		t.Errorf("missing final: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"final": "100"}); err == nil {
		t.Errorf("string final: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"final": 1, "extra": 1}); err == nil {
		t.Errorf("unknown key: expected error")
	}
}
