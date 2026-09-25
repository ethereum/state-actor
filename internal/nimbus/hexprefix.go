package nimbus

import "errors"

// Hex-prefix ("compact") encoding of a nibble path, as nim-eth's
// NibblesBuf.toHexPrefix: flag byte 0x00/0x10 (extension, even/odd length)
// or 0x20/0x30 (leaf), the first nibble packed into an odd flag byte, then
// two nibbles per byte. An empty path encodes as the flag byte alone.

// AppendHexPrefix appends the hex-prefix encoding of nibbles to dst.
func AppendHexPrefix(dst, nibbles []byte, leaf bool) []byte {
	var flag byte
	start := 0
	if len(nibbles)%2 == 1 {
		flag = 0x10 | nibbles[0]
		start = 1
	}
	if leaf {
		flag |= 0x20
	}
	dst = append(dst, flag)
	rest := nibbles[start:]
	for i := 0; i+1 < len(rest); i += 2 {
		dst = append(dst, rest[i]<<4|rest[i+1])
	}
	return dst
}

// HexPrefixLen returns len(AppendHexPrefix(nil, nibbles, _)) without encoding.
func HexPrefixLen(nibbleCount int) int {
	return 1 + nibbleCount/2
}

var errHexPrefix = errors.New("nimbus: malformed hex-prefix path")

// DecodeHexPrefix reverses AppendHexPrefix.
func DecodeHexPrefix(b []byte) (nibbles []byte, leaf bool, err error) {
	if len(b) == 0 {
		return nil, false, errHexPrefix
	}
	flag := b[0]
	if flag&0xc0 != 0 {
		return nil, false, errHexPrefix
	}
	leaf = flag&0x20 != 0
	odd := flag&0x10 != 0
	nibbles = make([]byte, 0, 2*len(b))
	if odd {
		nibbles = append(nibbles, flag&0x0f)
	} else if flag&0x0f != 0 {
		return nil, false, errHexPrefix
	}
	for _, c := range b[1:] {
		nibbles = append(nibbles, c>>4, c&0x0f)
	}
	return nibbles, leaf, nil
}
