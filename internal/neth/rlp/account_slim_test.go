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

// TestEncodeAccountSlim_Empty pins the slim RLP of an empty account: with the
// storage root == empty-trie root and code hash == empty-code hash, both are
// substituted with the RLP empty string (0x80), yielding the 4-item list
// [0x80, 0x80, 0x80, 0x80] → 0xc4 80 80 80 80.
func TestEncodeAccountSlim_Empty(t *testing.T) {
	acc := &types.StateAccount{
		Nonce:    0,
		Balance:  uint256.NewInt(0),
		Root:     common.Hash(neth.EmptyTreeHash),
		CodeHash: neth.OfAnEmptyString.Bytes(),
	}
	got := EncodeAccountSlim(acc)
	want, _ := hex.DecodeString("c480808080")
	if !bytes.Equal(got, want) {
		t.Fatalf("slim empty = %x want c480808080", got)
	}
}

// TestAccountSlim_NonEmpty checks that a contract account (non-empty storage
// root and code hash) keeps both 32-byte hashes in the slim form.
func TestAccountSlim_NonEmpty(t *testing.T) {
	root := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	codeHash := common.HexToHash("0xaabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")
	acc := &types.StateAccount{
		Nonce:    7,
		Balance:  uint256.NewInt(1234567),
		Root:     root,
		CodeHash: codeHash.Bytes(),
	}
	slim := EncodeAccountSlim(acc)
	if !bytes.Contains(slim, root[:]) {
		t.Errorf("slim form missing 32-byte storage root")
	}
	if !bytes.Contains(slim, codeHash[:]) {
		t.Errorf("slim form missing 32-byte code hash")
	}
}

// TestAccountSlim_MixedShapes covers the two independent-substitution shapes
// between empty and full: the common contract shape (empty storage root but
// non-empty code hash) and its inverse (non-empty storage root but empty code
// hash). SlimAccountRLP substitutes each field independently, so each is a
// distinct branch.
func TestAccountSlim_MixedShapes(t *testing.T) {
	root := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	codeHash := common.HexToHash("0xaabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")

	// Empty storage root + non-empty code hash (deployed code, no storage yet):
	// slim keeps the code hash and substitutes 0x80 for the empty storage root.
	codeOnly := &types.StateAccount{Nonce: 3, Balance: uint256.NewInt(1), Root: common.Hash(neth.EmptyTreeHash), CodeHash: codeHash.Bytes()}
	slim := EncodeAccountSlim(codeOnly)
	if !bytes.Contains(slim, codeHash[:]) {
		t.Errorf("code-only slim missing the 32-byte code hash: %x", slim)
	}
	if bytes.Contains(slim, neth.EmptyTreeHash[:]) {
		t.Errorf("code-only slim should substitute the empty storage root with 0x80, not embed it: %x", slim)
	}

	// Non-empty storage root + empty code hash (storage but no code): slim keeps
	// the storage root and substitutes 0x80 for the empty code hash.
	storageOnly := &types.StateAccount{Nonce: 4, Balance: uint256.NewInt(2), Root: root, CodeHash: neth.OfAnEmptyString.Bytes()}
	slim = EncodeAccountSlim(storageOnly)
	if !bytes.Contains(slim, root[:]) {
		t.Errorf("storage-only slim missing the 32-byte storage root: %x", slim)
	}
	if bytes.Contains(slim, neth.OfAnEmptyString[:]) {
		t.Errorf("storage-only slim should substitute the empty code hash with 0x80: %x", slim)
	}
}
