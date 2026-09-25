package nimbus

import (
	"errors"
	"fmt"

	"github.com/holiman/uint256"
)

// SBE ("short big-endian") is Aristo's integer encoding: big-endian bytes with
// the leading zero bytes dropped, but never shorter than one byte
// (aristo_blobify.nim blobify/significantBytesBE).

// SBELen returns the encoded length of v (1..8).
func SBELen(v uint64) int {
	n := 1
	for v >= 1<<8 {
		v >>= 8
		n++
	}
	return n
}

// AppendSBE appends the SBE encoding of v to dst.
func AppendSBE(dst []byte, v uint64) []byte {
	n := SBELen(v)
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, byte(v>>(8*uint(i))))
	}
	return dst
}

// AppendSBE256 appends the SBE encoding of v (nil or zero → one 0x00 byte).
func AppendSBE256(dst []byte, v *uint256.Int) []byte {
	if v == nil || v.IsZero() {
		return append(dst, 0)
	}
	return append(dst, v.Bytes()...)
}

// AppendSBEBytes32 appends the SBE encoding of a raw big-endian 32-byte value:
// leading zero bytes trimmed, at least one byte kept.
func AppendSBEBytes32(dst []byte, v [32]byte) []byte {
	i := 0
	for i < 31 && v[i] == 0 {
		i++
	}
	return append(dst, v[i:]...)
}

var errSBELen = errors.New("nimbus: SBE value must be 1..8 bytes")

// DecodeSBE decodes a 1..8-byte SBE integer.
func DecodeSBE(b []byte) (uint64, error) {
	if len(b) < 1 || len(b) > 8 {
		return 0, fmt.Errorf("%w (got %d)", errSBELen, len(b))
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// DecodeSBE256 decodes a 1..32-byte SBE integer.
func DecodeSBE256(b []byte) (*uint256.Int, error) {
	if len(b) < 1 || len(b) > 32 {
		return nil, fmt.Errorf("nimbus: SBE256 value must be 1..32 bytes (got %d)", len(b))
	}
	return new(uint256.Int).SetBytes(b), nil
}
