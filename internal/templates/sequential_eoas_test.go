package templates

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

func TestSequentialEOAsExpand(t *testing.T) {
	anchor := common.HexToAddress("0x0000000000000000000000000000000000001000")
	ent := mkContractEntity("sequential_eoas", map[string]any{
		"count":   5,
		"balance": "1000000000000000000",
	})
	ctx := Context{ResolvedAddress: anchor}

	out, err := (&sequentialEOAsTemplate{}).Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("count: got %d, want 5", len(out))
	}
	for i, pe := range out {
		want := common.BigToAddress(new(uint256.Int).SetUint64(0x1000 + uint64(i)).ToBig())
		if pe.Address != want {
			t.Errorf("addr[%d]: got %s, want %s", i, pe.Address.Hex(), want.Hex())
		}
		if pe.Account.Nonce != 0 {
			t.Errorf("addr[%d] nonce: got %d, want 0", i, pe.Account.Nonce)
		}
		if pe.Account.Balance.Uint64() != 1_000_000_000_000_000_000 {
			t.Errorf("addr[%d] balance: got %s", i, pe.Account.Balance)
		}
		if pe.Code != nil {
			t.Errorf("addr[%d]: Code must be nil for plain EOA", i)
		}
		if pe.Storage != nil {
			t.Errorf("addr[%d]: Storage must be nil for plain EOA", i)
		}
	}
}

func TestSequentialEOAsZeroCount(t *testing.T) {
	ent := mkContractEntity("sequential_eoas", map[string]any{"count": 0})
	out, err := (&sequentialEOAsTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected zero output for count=0, got %d", len(out))
	}
}

func TestSequentialEOAsRangeOverflow(t *testing.T) {
	// Start near the top of 20-byte address space; count=10 overflows.
	anchor := common.HexToAddress("0xfffffffffffffffffffffffffffffffffffffff8")
	ent := mkContractEntity("sequential_eoas", map[string]any{"count": 10})
	_, err := (&sequentialEOAsTemplate{}).Expand(Context{ResolvedAddress: anchor}, ent)
	if err == nil {
		t.Fatalf("expected overflow error, got nil")
	}
}

func TestSequentialEOAsValidate(t *testing.T) {
	tmpl := &sequentialEOAsTemplate{}
	if err := tmpl.ValidateParameters(map[string]any{"count": 100}); err != nil {
		t.Errorf("valid params rejected: %v", err)
	}
	if err := tmpl.ValidateParameters(map[string]any{}); err == nil {
		t.Errorf("missing count: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"count": "100"}); err == nil {
		t.Errorf("string count: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"count": 1, "wat": "x"}); err == nil {
		t.Errorf("unknown key: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"count": -1}); err == nil {
		t.Errorf("negative count: expected error")
	}
}
