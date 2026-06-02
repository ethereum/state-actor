package ethrex

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// EncodeAccountState returns the RLP encoding of an ethrex AccountState:
//
//	RLP([nonce, balance, storage_root, code_hash])
//
// Field order matches ethrex account.rs:219-227 (nonce, balance, storage_root, code_hash).
// The encoding is always the full 4-field form (never the "slim" compact form).
//
// Golden check:
//
//	nonce=7, balance=0x3635c9adc5dea00000, emptyStorageRoot, emptyCodeHash
//	-> 0xf84d07893635c9adc5dea00000a056e81f171b...a0c5d2460186f7...
func EncodeAccountState(nonce uint64, balance *uint256.Int, storageRoot, codeHash common.Hash) []byte {
	encNonce := rlpEncodeUint64(nonce)
	encBal := rlpEncodeBigInt(balance.ToBig())
	encStorage := rlpEncodeBytes(storageRoot[:])
	encCode := rlpEncodeBytes(codeHash[:])

	payload := make([]byte, 0, len(encNonce)+len(encBal)+len(encStorage)+len(encCode))
	payload = append(payload, encNonce...)
	payload = append(payload, encBal...)
	payload = append(payload, encStorage...)
	payload = append(payload, encCode...)
	return rlpEncodeListRaw(payload)
}

// EncodeStorageValue returns the RLP encoding of a non-zero storage slot value.
// The value is encoded as a minimal big-endian integer (stripping leading zeros).
// Callers must skip zero values — do not call this for v == 0.
func EncodeStorageValue(v *uint256.Int) []byte {
	return rlpEncodeBigInt(v.ToBig())
}

// rlpEncodeUint64 encodes a uint64 as an RLP integer (minimal big-endian, no leading zeros).
func rlpEncodeUint64(n uint64) []byte {
	if n == 0 {
		return []byte{0x80} // RLP encoding of zero integer
	}
	b := minBEBytesU64(n)
	return rlpEncodeBytes(b)
}

// rlpEncodeBigInt encodes a *big.Int as an RLP integer (minimal big-endian).
// Negative values and zero produce 0x80 (RLP null/zero).
func rlpEncodeBigInt(n *big.Int) []byte {
	if n == nil || n.Sign() <= 0 {
		return []byte{0x80}
	}
	b := n.Bytes() // big-endian, no leading zeros
	return rlpEncodeBytes(b)
}

// minBEBytesU64 returns the minimal big-endian encoding of a uint64.
func minBEBytesU64(n uint64) []byte {
	var buf [8]byte
	i := 7
	for n > 0 {
		buf[i] = byte(n)
		n >>= 8
		i--
	}
	return buf[i+1:]
}
