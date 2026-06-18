package ethrex

const (
	// LeafFlag is the nibble appended to a full path to mark it as a leaf key.
	LeafFlag = byte(16)
	// StorageSeparator is the nibble used to separate the address prefix from
	// the storage node path in storage_trie_nodes keys.
	StorageSeparator = byte(17)
)

// BytesToNibbles unpacks each byte into two nibbles (high nibble first, low nibble second).
// No leaf flag is appended. Example: [0x2f] -> [0x02, 0x0f].
func BytesToNibbles(b []byte) []byte {
	out := make([]byte, len(b)*2)
	for i, by := range b {
		out[i*2] = by >> 4
		out[i*2+1] = by & 0x0f
	}
	return out
}

// CompactEncode applies the hex-prefix (compact) encoding to a nibble slice.
// Mirrors ethrex nibbles.rs encode_compact.
//
// The flag byte encodes the node type and path-length parity:
//   - extension even: 0x00
//   - extension odd:  0x10 | first_nibble
//   - leaf even:      0x20
//   - leaf odd:       0x30 | first_nibble
//
// After the flag byte, remaining nibble pairs are packed two-per-byte.
func CompactEncode(nibbles []byte, isLeaf bool) []byte {
	odd := len(nibbles)%2 != 0

	var flagByte byte
	start := 0
	if odd {
		// First nibble is packed into the low nibble of the flag byte.
		flagByte = 0x10 | nibbles[0]
		start = 1
	}
	if isLeaf {
		flagByte |= 0x20
	}

	rest := nibbles[start:]
	out := make([]byte, 1+len(rest)/2)
	out[0] = flagByte
	for i := 0; i < len(rest)/2; i++ {
		out[1+i] = (rest[i*2] << 4) | rest[i*2+1]
	}
	return out
}

// commonPrefixLen returns the number of leading nibbles that a and b share.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
