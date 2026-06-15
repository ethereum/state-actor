package rlp

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/besu"
)

// TestEncodeAccount_EOAZero pins the byte output of a zero-state EOA:
// nonce=0, balance=0, storageRoot=EmptyTrieNodeHash, codeHash=EmptyCodeHash.
//
// Expected bytes (Besu BonsaiAccount.java:155-164 field order):
//
//	f844           — RLP list header, 68 bytes of content
//	80             — nonce scalar 0
//	80             — balance scalar 0
//	a0 56e81f...   — storageRoot (33 bytes: 0xa0 prefix + 32B)
//	a0 c5d246...   — codeHash   (33 bytes)
//
// Total = 2 + 1 + 1 + 33 + 33 = 70 bytes.
func TestEncodeAccount_EOAZero(t *testing.T) {
	got, err := EncodeAccount(
		0,
		uint256.NewInt(0),
		besu.EmptyTrieNodeHash,
		besu.EmptyCodeHash,
	)
	if err != nil {
		t.Fatalf("EncodeAccount: %v", err)
	}
	want, _ := hex.DecodeString(
		"f844" +
			"80" +
			"80" +
			"a0" + "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421" +
			"a0" + "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("EOA zero RLP mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}

// TestEncodeAccount_EOAFunded covers nonce=1, balance=1 ETH (10^18 wei).
// Confirms non-trivial balance encoding (0x0DE0B6B3A7640000 = 8 bytes → 0x88 prefix).
func TestEncodeAccount_EOAFunded(t *testing.T) {
	balance, _ := uint256.FromDecimal("1000000000000000000") // 1 ETH
	got, err := EncodeAccount(
		1,
		balance,
		besu.EmptyTrieNodeHash,
		besu.EmptyCodeHash,
	)
	if err != nil {
		t.Fatalf("EncodeAccount: %v", err)
	}
	want, _ := hex.DecodeString(
		"f84c" +
			"01" +
			"880de0b6b3a7640000" +
			"a0" + "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421" +
			"a0" + "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("funded EOA RLP mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}

// TestEncodeAccount_Contract covers a contract with explicit storageRoot and
// codeHash. Verifies those fields are always emitted as full 32-byte blobs.
func TestEncodeAccount_Contract(t *testing.T) {
	storageRoot := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	codeHash := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	got, err := EncodeAccount(
		42,
		uint256.NewInt(1234567890),
		storageRoot,
		codeHash,
	)
	if err != nil {
		t.Fatalf("EncodeAccount: %v", err)
	}
	// 1234567890 = 0x499602D2, 4 bytes, prefix 0x84.
	want, _ := hex.DecodeString(
		"f848" +
			"2a" +
			"84499602d2" +
			"a0" + "1111111111111111111111111111111111111111111111111111111111111111" +
			"a0" + "2222222222222222222222222222222222222222222222222222222222222222",
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("contract RLP mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}

// TestEncodeAccount_MaxUint256Balance verifies encoding for 2^256-1.
func TestEncodeAccount_MaxUint256Balance(t *testing.T) {
	maxBalance := new(uint256.Int).Sub(
		new(uint256.Int).Lsh(uint256.NewInt(1), 256),
		uint256.NewInt(1),
	)
	got, err := EncodeAccount(0, maxBalance, besu.EmptyTrieNodeHash, besu.EmptyCodeHash)
	if err != nil {
		t.Fatalf("EncodeAccount max balance: %v", err)
	}
	if len(got) != 102 {
		t.Fatalf("max-balance output length: got %d, want 102", len(got))
	}
}

// TestEncodeAccount_NilBalance ensures nil balance is treated as zero (don't panic).
func TestEncodeAccount_NilBalance(t *testing.T) {
	got, err := EncodeAccount(0, nil, besu.EmptyTrieNodeHash, besu.EmptyCodeHash)
	if err != nil {
		t.Fatalf("EncodeAccount nil balance: %v", err)
	}
	want, _ := EncodeAccount(0, uint256.NewInt(0), besu.EmptyTrieNodeHash, besu.EmptyCodeHash)
	if !bytes.Equal(got, want) {
		t.Fatalf("nil balance != zero balance:\n  got:  %x\n  want: %x", got, want)
	}
}
