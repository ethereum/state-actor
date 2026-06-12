package templates

import (
	"strings"
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

// TestSequentialEOAsRejectsZeroCount pins the I3 fix: count=0 used to
// expand to zero entities with zero warnings — a prestate silently
// missing the entity (same silent-zero family as the --seed=0 footgun).
// Both entry points now reject it.
func TestSequentialEOAsRejectsZeroCount(t *testing.T) {
	tmpl := &sequentialEOAsTemplate{}
	params := map[string]any{"count": 0}
	if err := tmpl.ValidateParameters(params); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("ValidateParameters(count=0): want 'must be >= 1' error, got %v", err)
	}
	if _, err := tmpl.Expand(Context{}, mkContractEntity("sequential_eoas", params)); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("Expand(count=0): want 'must be >= 1' error, got %v", err)
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
	if err := tmpl.ValidateParameters(map[string]any{"count": 0}); err == nil {
		t.Errorf("zero count: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"count": uint64(1)<<32 + 1}); err == nil {
		t.Errorf("count above 2^32: expected error (cap now enforced at validate time)")
	}
	// balance: omitted is valid (defaults to 1 in Expand).
	if err := tmpl.ValidateParameters(map[string]any{"count": 1}); err != nil {
		t.Errorf("omitted balance: rejected when it should default: %v", err)
	}
	// balance: "0" is rejected — zero-balance plain EOAs are pruned
	// by EIP-161, defeating the point of planting them.
	if err := tmpl.ValidateParameters(map[string]any{"count": 1, "balance": "0"}); err == nil {
		t.Errorf("balance=0: expected error")
	}
	if err := tmpl.ValidateParameters(map[string]any{"count": 1, "balance": "0x0"}); err == nil {
		t.Errorf("balance=0x0: expected error")
	}
	// balance: "1" is the minimum accepted value.
	if err := tmpl.ValidateParameters(map[string]any{"count": 1, "balance": "1"}); err != nil {
		t.Errorf("balance=1: rejected: %v", err)
	}
}

// TestSequentialEOAsDefaultBalanceIsOne pins the new default: omitting
// `balance` yields 1 wei per EOA, not 0. Default 0 would let EIP-161
// prune the planted accounts before the benchmark could reference them.
func TestSequentialEOAsDefaultBalanceIsOne(t *testing.T) {
	anchor := common.HexToAddress("0x0000000000000000000000000000000000002000")
	ent := mkContractEntity("sequential_eoas", map[string]any{"count": 4})
	out, err := (&sequentialEOAsTemplate{}).Expand(Context{ResolvedAddress: anchor}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("count: got %d, want 4", len(out))
	}
	for i, pe := range out {
		if got := pe.Account.Balance.Uint64(); got != 1 {
			t.Errorf("addr[%d] balance: got %d, want 1 (default)", i, got)
		}
	}
}

// TestSequentialEOAsRejectsZeroBalance pins both the validator and the
// Expand-time defense-in-depth check: balance="0" surfaces an error
// from each entry point.
func TestSequentialEOAsRejectsZeroBalance(t *testing.T) {
	tmpl := &sequentialEOAsTemplate{}
	params := map[string]any{"count": 3, "balance": "0"}
	if err := tmpl.ValidateParameters(params); err == nil {
		t.Errorf("ValidateParameters(balance=0): expected error, got nil")
	}
	// Skip the validator and hit Expand directly to confirm the
	// secondary check also fires.
	anchor := common.HexToAddress("0x0000000000000000000000000000000000003000")
	ent := mkContractEntity("sequential_eoas", params)
	if _, err := tmpl.Expand(Context{ResolvedAddress: anchor}, ent); err == nil {
		t.Errorf("Expand(balance=0): expected error, got nil")
	}
}
