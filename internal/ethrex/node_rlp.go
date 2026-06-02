package ethrex

// node_rlp.go — RLP encoders for ethrex trie node types.
//
// All three encoders mirror ethrex crates/common/trie/rlp.rs lines 26-85.
// The encoding is used both for DB storage and for computing NodeRef hashes.

// EncodeLeaf encodes a leaf node as a 2-item RLP list:
//
//	RLP([CompactEncode(remainingPath, true), value])
//
// Mirrors ethrex LeafNode RLPEncode (rlp.rs:78-85).
func EncodeLeaf(remainingPath []byte, value []byte) []byte {
	compact := CompactEncode(remainingPath, true)
	encPath := rlpEncodeBytes(compact)
	encVal := rlpEncodeBytes(value)
	payload := append(encPath, encVal...)
	return rlpEncodeListRaw(payload)
}

// EncodeExtension encodes an extension node as a 2-item RLP list:
//
//	RLP([CompactEncode(prefixPath, false), <child reference>])
//
// childEncoded is the raw RLP of the child node (already encoded). The child
// is embedded via the NodeRef rule (inline if < 32 bytes, hash-ref if >= 32).
// Mirrors ethrex ExtensionNode RLPEncode (rlp.rs:70-76).
func EncodeExtension(prefixPath []byte, childEncoded []byte) []byte {
	compact := CompactEncode(prefixPath, false)
	encPath := rlpEncodeBytes(compact)

	ref := NodeRef(childEncoded)
	var childRLP []byte
	childRLP = appendChildRef(childRLP, ref)

	payload := append(encPath, childRLP...)
	return rlpEncodeListRaw(payload)
}

// EncodeBranch encodes a branch node as a 17-item RLP list.
//
// Items 0..15 are child references: each child's encoding is passed through
// NodeRef and then appended via appendChildRef. A nil or zero-length entry
// encodes as RLP empty string 0x80. Item 16 is the branch value (RLP byte
// string, 0x80 if empty).
//
// children[i] is the raw RLP of child i (may be nil if the slot is empty).
// Mirrors ethrex BranchNode RLPEncode (rlp.rs:26-68).
func EncodeBranch(children [16][]byte, value []byte) []byte {
	var payload []byte
	for i := 0; i < 16; i++ {
		child := children[i]
		if len(child) == 0 {
			payload = append(payload, 0x80)
		} else {
			ref := NodeRef(child)
			payload = appendChildRef(payload, ref)
		}
	}
	// Branch value (item 16).
	payload = append(payload, rlpEncodeBytes(value)...)
	return rlpEncodeListRaw(payload)
}

// ---------------------------------------------------------------------------
// Minimal RLP helpers (no external RLP library dependency in production code)
// ---------------------------------------------------------------------------

// rlpEncodeBytes encodes a byte slice as an RLP byte string.
func rlpEncodeBytes(b []byte) []byte {
	n := len(b)
	switch {
	case n == 1 && b[0] < 0x80:
		// Single byte in [0x00, 0x7f]: encode as itself.
		return []byte{b[0]}
	case n == 0:
		return []byte{0x80}
	case n <= 55:
		out := make([]byte, 1+n)
		out[0] = 0x80 + byte(n)
		copy(out[1:], b)
		return out
	default:
		lenBytes := minBEBytes(n)
		out := make([]byte, 1+len(lenBytes)+n)
		out[0] = 0xb7 + byte(len(lenBytes))
		copy(out[1:], lenBytes)
		copy(out[1+len(lenBytes):], b)
		return out
	}
}

// rlpEncodeListRaw wraps a pre-built payload in an RLP list header.
func rlpEncodeListRaw(payload []byte) []byte {
	n := len(payload)
	if n <= 55 {
		out := make([]byte, 1+n)
		out[0] = 0xc0 + byte(n)
		copy(out[1:], payload)
		return out
	}
	lenBytes := minBEBytes(n)
	out := make([]byte, 1+len(lenBytes)+n)
	out[0] = 0xf7 + byte(len(lenBytes))
	copy(out[1:], lenBytes)
	copy(out[1+len(lenBytes):], payload)
	return out
}

// minBEBytes returns the minimal big-endian encoding of n (n > 0).
func minBEBytes(n int) []byte {
	var buf [8]byte
	i := 7
	for n > 0 {
		buf[i] = byte(n)
		n >>= 8
		i--
	}
	return buf[i+1:]
}
