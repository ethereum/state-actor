package templates

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/spec"
)

func TestRawTemplateBasic(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	code := []byte{0x60, 0x80, 0x60, 0x40}

	ent := spec.Entity{
		Kind: spec.KindContract,
		Code: code,
	}
	ctx := Context{
		Seed:            42,
		ClientName:      "geth",
		Sizer:           fixedSizer{bytesPerSlot: 64},
		ResolvedAddress: addr,
	}

	out, err := (&rawTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 PreAllocEntity, got %d", len(out))
	}
	pe := out[0]

	if pe.Address != addr {
		t.Errorf("address: got %v, want %v", pe.Address, addr)
	}
	if pe.Account.Nonce != 0 {
		t.Errorf("nonce: got %d, want 0", pe.Account.Nonce)
	}
	wantCH := crypto.Keccak256Hash(code).Bytes()
	if string(pe.Account.CodeHash) != string(wantCH) {
		t.Errorf("CodeHash mismatch")
	}
	if string(pe.Code) != string(code) {
		t.Errorf("Code not passed through")
	}
	if pe.Storage != nil {
		t.Errorf("Storage should be nil without approximate_size_bytes")
	}
}

func TestRawTemplateWithStorage(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000002")
	ent := spec.Entity{
		Kind:                 spec.KindContract,
		Code:                 []byte{0x00},
		ApproximateSizeBytes: 6400, // ÷ 64 bytes/slot = 100 slots
	}
	ctx := Context{
		Seed:            7,
		ClientName:      "reth",
		Sizer:           fixedSizer{bytesPerSlot: 64},
		ResolvedAddress: addr,
	}

	out, err := (&rawTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if out[0].Storage == nil {
		t.Fatal("expected non-nil Storage")
	}
	pairs := collectPairs(out[0].Storage)
	if len(pairs) != 100 {
		t.Errorf("slot count: got %d, want 100", len(pairs))
	}
}

func TestRawTemplateRejectsNoCode(t *testing.T) {
	ent := spec.Entity{Kind: spec.KindContract}
	ctx := Context{Sizer: fixedSizer{bytesPerSlot: 64}}
	if _, err := (&rawTemplate{}).Expand(ctx, ent); err == nil {
		t.Error("expected error when code is empty")
	}
}

func TestRawTemplateRejectsParameters(t *testing.T) {
	if err := (&rawTemplate{}).ValidateParameters(map[string]any{"foo": "bar"}); err == nil {
		t.Error("expected error for non-empty parameters")
	}
}
