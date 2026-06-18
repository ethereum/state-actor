package ethrex

import "github.com/ethereum/go-ethereum/crypto"

// NodeRef returns the child-reference encoding for an already-encoded trie node.
// Mirrors ethrex node_hash.rs NodeHash::from_encoded:
//   - if len(encodedNode) < 32: return the raw encoded bytes (inline embedding).
//   - if len(encodedNode) >= 32: return keccak256(encodedNode) as a 32-byte hash.
//
// The returned slice is either the raw RLP (< 32 bytes) or a 32-byte keccak hash.
// Callers use appendChildRef to embed the result into a parent node's RLP payload.
func NodeRef(encodedNode []byte) []byte {
	if len(encodedNode) < 32 {
		out := make([]byte, len(encodedNode))
		copy(out, encodedNode)
		return out
	}
	return crypto.Keccak256(encodedNode)
}

// appendChildRef appends the RLP encoding of a child reference to dst.
//
// The encoding rules follow ethrex node_hash.rs NodeHash::encode:
//   - absent child (nil or empty): append RLP empty string 0x80.
//   - inline child (len < 32): the raw bytes are already valid RLP (they ARE the
//     inline node RLP), so splice them directly without re-encoding.
//   - hashed child (len == 32): encode as a 32-byte RLP byte string (0xa0 prefix).
func appendChildRef(dst []byte, ref []byte) []byte {
	switch len(ref) {
	case 0:
		return append(dst, 0x80)
	case 32:
		// 32-byte hash: RLP byte-string encoding = 0xa0 || hash.
		dst = append(dst, 0xa0)
		dst = append(dst, ref...)
	default:
		// Inline: the ref is already valid RLP; splice directly.
		dst = append(dst, ref...)
	}
	return dst
}
