package ethrex

import (
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
	encBal := rlpEncodeUint256(balance)
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
	return rlpEncodeUint256(v)
}

// EncodeStorageValueBytes32 is EncodeStorageValue for a raw 32-byte value:
// leading zeros are trimmed by subslicing — no intermediate integer types.
// Byte-identical to EncodeStorageValue(new(uint256.Int).SetBytes32(v[:])).
// Callers must skip zero values.
func EncodeStorageValueBytes32(v common.Hash) []byte {
	i := 0
	for i < 32 && v[i] == 0 {
		i++
	}
	return rlpEncodeBytes(v[i:])
}

// StorageValueRLPLength returns len(EncodeStorageValueBytes32(v)) without
// encoding — for stat accounting that only needs the byte count.
func StorageValueRLPLength(v common.Hash) int {
	i := 0
	for i < 32 && v[i] == 0 {
		i++
	}
	n := 32 - i
	switch {
	case n == 1 && v[i] < 0x80:
		return 1
	case n == 0:
		return 1 // 0x80
	default:
		return 1 + n // n <= 32 <= 55: single length-prefix byte
	}
}

// rlpEncodeUint64 encodes a uint64 as an RLP integer (minimal big-endian, no leading zeros).
func rlpEncodeUint64(n uint64) []byte {
	if n == 0 {
		return []byte{0x80} // RLP encoding of zero integer
	}
	b := minBEBytesU64(n)
	return rlpEncodeBytes(b)
}

// rlpEncodeUint256 encodes a *uint256.Int as an RLP integer (minimal
// big-endian). nil and zero produce 0x80 (RLP null/zero). Byte-identical to
// the previous big.Int round trip (uint256.Bytes() is minimal big-endian)
// without the ToBig allocation ladder.
func rlpEncodeUint256(n *uint256.Int) []byte {
	if n == nil || n.IsZero() {
		return []byte{0x80}
	}
	return rlpEncodeBytes(n.Bytes())
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
