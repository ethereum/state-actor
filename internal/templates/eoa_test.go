package templates

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func TestEOATemplatePlainEOA(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000003")
	bal := &spec.BigIntDecimal{V: uint256.NewInt(1_000_000_000_000_000_000)}

	ent := spec.Entity{Kind: spec.KindEOA, Balance: bal, Nonce: 5}
	ctx := Context{Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: addr}

	out, err := (&eoaTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	pe := out[0]
	if pe.Account.Nonce != 5 {
		t.Errorf("nonce: got %d, want 5", pe.Account.Nonce)
	}
	if !pe.Account.Balance.Eq(bal.V) {
		t.Errorf("balance: got %s, want %s", pe.Account.Balance, bal.V)
	}
	if pe.Code != nil {
		t.Errorf("Code should be nil for plain EOA, got %x", pe.Code)
	}
	if string(pe.Account.CodeHash) != string(types.EmptyCodeHash[:]) {
		t.Errorf("CodeHash should be EmptyCodeHash for plain EOA")
	}
	if pe.Storage != nil {
		t.Errorf("Storage should be nil for plain EOA")
	}
}

func TestEOATemplate7702Delegation(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000004")
	delegationCode := append([]byte{0xef, 0x01, 0x00}, make([]byte, 20)...)
	for i := 0; i < 20; i++ {
		delegationCode[3+i] = 0xaa
	}

	ent := spec.Entity{Kind: spec.KindEOA, Code: delegationCode}
	ctx := Context{Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: addr}

	out, err := (&eoaTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out[0].Code) != 23 {
		t.Errorf("7702 code length: got %d, want 23", len(out[0].Code))
	}
	if string(out[0].Account.CodeHash) == string(types.EmptyCodeHash[:]) {
		t.Errorf("CodeHash should reflect 7702 marker, not be EmptyCodeHash")
	}
}

func TestEOATemplateStorageBloat(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000005")
	ent := spec.Entity{
		Kind:                 spec.KindEOA,
		ApproximateSizeBytes: 64_000, // ÷ 64 = 1000 slots
	}
	ctx := Context{
		Seed:            123,
		ClientName:      "besu",
		Sizer:           fixedSizer{bytesPerSlot: 64},
		ResolvedAddress: addr,
	}

	out, err := (&eoaTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	pairs := collectPairs(out[0].Storage)
	if len(pairs) != 1000 {
		t.Errorf("synthesized slot count: got %d, want 1000", len(pairs))
	}
}
