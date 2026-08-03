package ethrex

import (
	"sync"

	"github.com/ethereum/go-ethereum/crypto"
)

// node_rlp.go — RLP encoders for ethrex trie node types.
//
// All three encoders mirror ethrex crates/common/trie/rlp.rs lines 26-85.
// The encoding is used both for DB storage and for computing NodeRef hashes.

// nodeEncoder holds the per-encode scratch state: compact/payload build
// buffers and a reused keccak hasher for child refs. Outputs of the three
// Encode* functions are always FRESHLY allocated (rlpEncodeListRaw copies the
// payload), so callers may retain them; only the intermediates are pooled.
// Pooled because encoders run concurrently (Phase-0 workers + Stage-B workers
// each drive their own Builder).
type nodeEncoder struct {
	compact []byte
	payload []byte
	out32   [32]byte
	h       crypto.KeccakState
}

var encoderPool = sync.Pool{New: func() any {
	return &nodeEncoder{h: crypto.NewKeccakState()}
}}

// appendChildRefOf appends the child-reference encoding of child to dst:
// byte-identical to appendChildRef(dst, NodeRef(child)) but hashing into the
// encoder's scratch instead of allocating a KeccakState + 32-byte digest per
// child (geth's hasher.hashDataTo precedent).
func (e *nodeEncoder) appendChildRefOf(dst, child []byte) []byte {
	if len(child) < 32 {
		// Inline: the raw bytes are already valid RLP; splice directly.
		return append(dst, child...)
	}
	e.h.Reset()
	e.h.Write(child)
	e.h.Read(e.out32[:])
	dst = append(dst, 0xa0)
	return append(dst, e.out32[:]...)
}

// EncodeLeaf encodes a leaf node as a 2-item RLP list:
//
//	RLP([CompactEncode(remainingPath, true), value])
//
// Mirrors ethrex LeafNode RLPEncode (rlp.rs:78-85).
func EncodeLeaf(remainingPath []byte, value []byte) []byte {
	e := encoderPool.Get().(*nodeEncoder)
	e.compact = appendCompact(e.compact[:0], remainingPath, true)
	p := e.payload[:0]
	p = appendRLPBytes(p, e.compact)
	p = appendRLPBytes(p, value)
	out := rlpEncodeListRaw(p)
	e.payload = p
	encoderPool.Put(e)
	return out
}

// EncodeExtension encodes an extension node as a 2-item RLP list:
//
//	RLP([CompactEncode(prefixPath, false), <child reference>])
//
// childEncoded is the raw RLP of the child node (already encoded). The child
// is embedded via the NodeRef rule (inline if < 32 bytes, hash-ref if >= 32).
// Mirrors ethrex ExtensionNode RLPEncode (rlp.rs:70-76).
func EncodeExtension(prefixPath []byte, childEncoded []byte) []byte {
	e := encoderPool.Get().(*nodeEncoder)
	e.compact = appendCompact(e.compact[:0], prefixPath, false)
	p := e.payload[:0]
	p = appendRLPBytes(p, e.compact)
	p = e.appendChildRefOf(p, childEncoded)
	out := rlpEncodeListRaw(p)
	e.payload = p
	encoderPool.Put(e)
	return out
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
	e := encoderPool.Get().(*nodeEncoder)
	// Preallocate for the worst case (16 hashed refs + value) once; the pooled
	// buffer retains its capacity, so steady state is zero-alloc here where the
	// previous `var payload []byte` paid ~10 append-grow reallocs per branch.
	p := e.payload[:0]
	if cap(p) < 17*33+8 {
		p = make([]byte, 0, 17*33+8)
	}
	for i := 0; i < 16; i++ {
		child := children[i]
		if len(child) == 0 {
			p = append(p, 0x80)
		} else {
			p = e.appendChildRefOf(p, child)
		}
	}
	// Branch value (item 16).
	p = appendRLPBytes(p, value)
	out := rlpEncodeListRaw(p)
	e.payload = p
	encoderPool.Put(e)
	return out
}

// ---------------------------------------------------------------------------
// Minimal RLP helpers (no external RLP library dependency in production code)
// ---------------------------------------------------------------------------

// appendRLPBytes is rlpEncodeBytes in append form; byte-identical output.
func appendRLPBytes(dst, b []byte) []byte {
	n := len(b)
	switch {
	case n == 1 && b[0] < 0x80:
		return append(dst, b[0])
	case n == 0:
		return append(dst, 0x80)
	case n <= 55:
		dst = append(dst, 0x80+byte(n))
		return append(dst, b...)
	default:
		lenBytes := minBEBytes(n)
		dst = append(dst, 0xb7+byte(len(lenBytes)))
		dst = append(dst, lenBytes...)
		return append(dst, b...)
	}
}

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
