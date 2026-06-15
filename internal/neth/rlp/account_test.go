package rlp

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/neth"
)

// TestEncodeAccount_Empty pins the byte output of an empty account
// (zero nonce, zero balance, empty-tree storage root, empty-string code
// hash). Any drift here means our Account RLP doesn't match Nethermind's.
//
// Expected payload:
//
//	[0]  RLP list header (length 68 → 0xf8 0x44)
//	[2]  Nonce = 0    → 0x80
//	[3]  Balance = 0  → 0x80
//	[4]  StorageRoot  → 0xa0 || <32 bytes EmptyTreeHash>
//	[37] CodeHash     → 0xa0 || <32 bytes OfAnEmptyString>
func TestEncodeAccount_Empty(t *testing.T) {
	acc := &types.StateAccount{
		Nonce:    0,
		Balance:  uint256.NewInt(0),
		Root:     common.Hash(neth.EmptyTreeHash),
		CodeHash: neth.OfAnEmptyString.Bytes(),
	}

	got, err := EncodeAccount(acc)
	if err != nil {
		t.Fatalf("EncodeAccount: %v", err)
	}

	want, _ := hex.DecodeString(
		"f844" + // list header (68 bytes content)
			"80" + // nonce 0
			"80" + // balance 0
			"a0" + "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421" + // EmptyTreeHash
			"a0" + "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470", // OfAnEmptyString
	)

	if !bytes.Equal(got, want) {
		t.Fatalf("empty-account RLP mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}

// TestEncodeAccount_Funded covers a non-empty account: nonce=1, balance=1
// ETH (10^18 wei), empty storage/code. The 1-ETH balance is the
// canonical "this is a normal EOA" fixture; pinning it confirms balance
// encoding agrees with Nethermind for non-trivial values.
func TestEncodeAccount_Funded(t *testing.T) {
	balance, _ := uint256.FromDecimal("1000000000000000000") // 1 ETH

	acc := &types.StateAccount{
		Nonce:    1,
		Balance:  balance,
		Root:     common.Hash(neth.EmptyTreeHash),
		CodeHash: neth.OfAnEmptyString.Bytes(),
	}

	got, err := EncodeAccount(acc)
	if err != nil {
		t.Fatalf("EncodeAccount: %v", err)
	}

	// 1 ETH in wei is 0x0DE0B6B3A7640000 — RLP-encodes as 0x88 + 8 bytes.
	want, _ := hex.DecodeString(
		"f84c" + // list header (76 bytes content)
			"01" + // nonce = 1
			"880de0b6b3a7640000" + // balance = 1 ETH
			"a0" + "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421" +
			"a0" + "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
	)

	if !bytes.Equal(got, want) {
		t.Fatalf("funded-account RLP mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}

// TestEncodeAccount_NonZeroStorageRootAndCodeHash uses arbitrary 32-byte
// values for storageRoot/codeHash to confirm those fields aren't
// accidentally treated as the empty markers (which would suppress them in
// slim form — but we never use slim form).
func TestEncodeAccount_NonZeroStorageRootAndCodeHash(t *testing.T) {
	storageRoot := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	codeHash := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")

	acc := &types.StateAccount{
		Nonce:    42,
		Balance:  uint256.NewInt(1234567890),
		Root:     storageRoot,
		CodeHash: codeHash.Bytes(),
	}

	got, err := EncodeAccount(acc)
	if err != nil {
		t.Fatalf("EncodeAccount: %v", err)
	}

	// 1234567890 (decimal) = 0x499602D2 (4 bytes), RLP = 0x84 + 4 bytes.
	want, _ := hex.DecodeString(
		"f848" + // list header (72 bytes content)
			"2a" + // nonce = 42 (single byte)
			"84499602d2" + // balance = 1234567890
			"a0" + "1111111111111111111111111111111111111111111111111111111111111111" +
			"a0" + "2222222222222222222222222222222222222222222222222222222222222222",
	)

	if !bytes.Equal(got, want) {
		t.Fatalf("non-empty-account RLP mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}
