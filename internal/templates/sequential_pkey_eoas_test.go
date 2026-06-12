package templates

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// TestSequentialPkeyEOAs_KnownPkeyPair pins the head of the canonical
// SENDER_BASE_KEY pool used by
// execution-specs/tests/benchmark/stateful/bloatnet/test_transaction_types.py
// (SENDER_BASE_KEY = 0x111…1). The first derived address must match
// what `EOA(key=SENDER_BASE_KEY).address` produces in EEST — namely
// 0x19E7E376E7C213B7E7E7E46cc70A5dD086DAff2A. If this pins fails, the
// senders this template plants no longer line up with what
// yield_distinct_sender() expects on-chain.
func TestSequentialPkeyEOAs_KnownPkeyPair(t *testing.T) {
	ent := mkContractEntity("sequential_pkey_eoas", map[string]any{
		"start_pkey": "0x1111111111111111111111111111111111111111111111111111111111111111",
		"count":      1,
	})
	out, err := (&sequentialPkeyEOAsTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("count: got %d, want 1", len(out))
	}
	want := common.HexToAddress("0x19E7E376E7C213B7E7E7E46cc70A5dD086DAff2A")
	if out[0].Address != want {
		t.Errorf("derived address: got %s, want %s", out[0].Address.Hex(), want.Hex())
	}
}

// TestSequentialPkeyEOAs_Sequential pins that successive derivations
// step through pkey 0x111…1 + i. Cross-checks each address against the
// authoritative go-ethereum derivation (crypto.PubkeyToAddress).
func TestSequentialPkeyEOAs_Sequential(t *testing.T) {
	ent := mkContractEntity("sequential_pkey_eoas", map[string]any{
		"start_pkey": "0x1111111111111111111111111111111111111111111111111111111111111111",
		"count":      8,
		"balance":    "1000000000000000000",
	})
	out, err := (&sequentialPkeyEOAsTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 8 {
		t.Fatalf("count: got %d, want 8", len(out))
	}
	base := new(big.Int).SetBytes(common.FromHex(
		"0x1111111111111111111111111111111111111111111111111111111111111111",
	))
	for i, pe := range out {
		pk := new(big.Int).Add(base, big.NewInt(int64(i)))
		priv, err := derivePrivateKey(pk)
		if err != nil {
			t.Fatalf("control derivation #%d: %v", i, err)
		}
		want := crypto.PubkeyToAddress(priv.PublicKey)
		if pe.Address != want {
			t.Errorf("entry #%d: got %s, want %s", i, pe.Address.Hex(), want.Hex())
		}
		if !pe.Account.Balance.Eq(uint256.NewInt(1_000_000_000_000_000_000)) {
			t.Errorf("entry #%d: balance mismatch", i)
		}
		if pe.Code != nil {
			t.Errorf("entry #%d: Code must be nil for plain EOA", i)
		}
		if pe.Account.Nonce != 0 {
			t.Errorf("entry #%d: nonce must be 0 (got %d)", i, pe.Account.Nonce)
		}
	}
}

// TestSequentialPkeyEOAs_DefaultBalance pins the EIP-161 dodge: when
// `balance` is omitted, plant 1 wei so the account survives empty-
// account pruning.
func TestSequentialPkeyEOAs_DefaultBalance(t *testing.T) {
	ent := mkContractEntity("sequential_pkey_eoas", map[string]any{
		"start_pkey": "0x1111111111111111111111111111111111111111111111111111111111111111",
		"count":      1,
	})
	out, err := (&sequentialPkeyEOAsTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !out[0].Account.Balance.Eq(uint256.NewInt(1)) {
		t.Errorf("default balance: got %s, want 1 wei", out[0].Account.Balance)
	}
}

// TestSequentialPkeyEOAs_RejectsZeroBalance pins the explicit-zero
// rejection (zero-balance plain EOAs are pruned by EIP-161 and leave
// no on-chain trace).
func TestSequentialPkeyEOAs_RejectsZeroBalance(t *testing.T) {
	err := (&sequentialPkeyEOAsTemplate{}).ValidateParameters(map[string]any{
		"start_pkey": "0x1111111111111111111111111111111111111111111111111111111111111111",
		"count":      5,
		"balance":    "0",
	})
	if err == nil {
		t.Fatal("expected balance=0 to be rejected")
	}
}

// TestSequentialPkeyEOAs_RejectsZeroPkey pins that pkey 0 is rejected
// — it's not a valid secp256k1 scalar.
func TestSequentialPkeyEOAs_RejectsZeroPkey(t *testing.T) {
	err := (&sequentialPkeyEOAsTemplate{}).ValidateParameters(map[string]any{
		"start_pkey": "0x0000000000000000000000000000000000000000000000000000000000000000",
		"count":      1,
	})
	if err == nil {
		t.Fatal("expected start_pkey=0 to be rejected")
	}
}

// TestSequentialPkeyEOAs_RejectsBadPkeyLength pins that anything other
// than exactly 32 bytes is rejected.
func TestSequentialPkeyEOAs_RejectsBadPkeyLength(t *testing.T) {
	cases := []string{
		"0x11",                                                                  // too short
		"0x111111111111111111111111111111111111111111111111111111111111111111",  // 33 bytes
	}
	for _, c := range cases {
		err := (&sequentialPkeyEOAsTemplate{}).ValidateParameters(map[string]any{
			"start_pkey": c,
			"count":      1,
		})
		if err == nil {
			t.Errorf("expected rejection for start_pkey=%s", c)
		}
	}
}

// TestSequentialPkeyEOAs_RejectsAboveCurveOrder pins that pkeys at or
// beyond the secp256k1 curve order are rejected.
func TestSequentialPkeyEOAs_RejectsAboveCurveOrder(t *testing.T) {
	// Exactly the curve order — invalid.
	err := (&sequentialPkeyEOAsTemplate{}).ValidateParameters(map[string]any{
		"start_pkey": "0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141",
		"count":      1,
	})
	if err == nil {
		t.Fatal("expected start_pkey at curve order to be rejected")
	}
}

// TestSequentialPkeyEOAs_RejectsMissingRequired pins the required-param
// check.
func TestSequentialPkeyEOAs_RejectsMissingRequired(t *testing.T) {
	cases := []map[string]any{
		{"count": 1},
		{"start_pkey": "0x1111111111111111111111111111111111111111111111111111111111111111"},
	}
	for i, c := range cases {
		if err := (&sequentialPkeyEOAsTemplate{}).ValidateParameters(c); err == nil {
			t.Errorf("case[%d]: expected missing-required-param error", i)
		}
	}
}

// TestSequentialPkeyEOAs_ZeroCountNoop pins that count=0 is a no-op
// (no error, no entries) — matches sequential_eoas semantics for
// consistency.
// TestSequentialPkeyEOAs_RejectsZeroCount pins the I3 fix: count=0 used
// to expand to zero entities with zero warnings (matching the old
// sequential_eoas semantics). Both entry points now reject it.
func TestSequentialPkeyEOAs_RejectsZeroCount(t *testing.T) {
	tmpl := &sequentialPkeyEOAsTemplate{}
	params := map[string]any{
		"start_pkey": "0x1111111111111111111111111111111111111111111111111111111111111111",
		"count":      0,
	}
	if err := tmpl.ValidateParameters(params); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("ValidateParameters(count=0): want 'must be >= 1' error, got %v", err)
	}
	if _, err := tmpl.Expand(Context{}, mkContractEntity("sequential_pkey_eoas", params)); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("Expand(count=0): want 'must be >= 1' error, got %v", err)
	}
}
