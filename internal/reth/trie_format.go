package reth

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
)

// StoredNibbles is a nibble path with a fixed-64-byte buffer + an
// explicit Length suffix. Two encoders use the same struct:
//
//   - EncodeKey: 65-byte form (nibbles[64] || length[1]). Used as the
//     StoragesTrie DupSort SubKey inside StorageTrieEntry. See reth-
//     trie-common nibbles.rs.
//   - EncodePackedAccountKey / EncodePackedCompact: 33-byte packed form
//     (two nibbles per byte, padded to 32 bytes + 1 nibble-count byte).
//     This is what reth v2's PackedKeyAdapter expects for AccountsTrie /
//     StoragesTrie reads.
type StoredNibbles struct {
	Length  byte
	Nibbles [64]byte
}

// EncodeKey writes the 65-byte form (nibbles[64] || length[1]).
func (s *StoredNibbles) EncodeKey(buf *bytes.Buffer) {
	buf.Write(s.Nibbles[:])
	buf.WriteByte(s.Length)
}

func (s *StoredNibbles) DecodeKey(b []byte) {
	if len(b) < 65 {
		panic("StoredNibbles: truncated key, need >=65 bytes")
	}
	copy(s.Nibbles[:], b[0:64])
	s.Length = b[64]
}

// StoredNibblesSubKey is the StoragesTrie DupSort sub-key (after the 32-byte
// hashed-address main key). Same 65-byte layout as StoredNibbles.
type StoredNibblesSubKey = StoredNibbles

// EncodePackedAccountKey writes the 33-byte packed form to buf — the wire
// layout reth's PackedAccountsTrie key uses under storage_v2. Two nibbles
// per byte (high nibble in upper 4 bits, low nibble in lower 4 bits), right-
// padded to 32 bytes; the 33rd byte carries the actual nibble count.
//
// Mirrors PackedStoredNibbles::to_compact_array in reth-trie-common
// (nibbles.rs).
//
// SAFE under storage_v2=true only — a non-v2 reader would decode this as a
// 33-NIBBLE path (one nibble per byte) and silently corrupt the trie.
func (s *StoredNibbles) EncodePackedAccountKey(buf *bytes.Buffer) {
	if int(s.Length) > 64 {
		panic("StoredNibbles: Length > 64")
	}
	var out [33]byte
	n := int(s.Length)
	pairs := n / 2
	for i := 0; i < pairs; i++ {
		out[i] = (s.Nibbles[2*i] << 4) | s.Nibbles[2*i+1]
	}
	if n%2 == 1 {
		out[pairs] = s.Nibbles[2*pairs] << 4
	}
	out[32] = s.Length
	buf.Write(out[:])
}

// BranchNodeCompact mirrors alloy_trie::BranchNodeCompact (alloy-trie 0.9.5,
// nodes/branch.rs). Used as the value of AccountsTrie / StoragesTrie.
//
// Wire format (cross-validated against Rust in golden_test.go; canonical
// source: reth-codecs 0.3.1 src/alloy/trie.rs lines 55-75):
//
//  1. state_mask: BE u16
//  2. tree_mask: BE u16
//  3. hash_mask: BE u16
//  4. root_hash: 32 bytes, present ONLY when RootHash != nil
//  5. hashes: popcount(hash_mask) × 32 bytes
//
// Note: there is NO bitflag header byte. root_hash presence is inferred from
// the total encoded length: if (totalLen-6) % 32 == 0 and the hash count
// equals popcount(hash_mask)+1, a root_hash precedes the child hashes.
type BranchNodeCompact struct {
	StateMask uint16
	TreeMask  uint16
	HashMask  uint16
	Hashes    []common.Hash
	RootHash  *common.Hash
}

func (b *BranchNodeCompact) EncodeCompact(buf *bytes.Buffer) int {
	if popcount16(b.HashMask) != len(b.Hashes) {
		panic("BranchNodeCompact: hash_mask popcount != len(hashes)")
	}
	if b.TreeMask&^b.StateMask != 0 || b.HashMask&^b.StateMask != 0 {
		panic("BranchNodeCompact: tree_mask/hash_mask must be subset of state_mask")
	}

	written := 0
	written += writeBEU16(buf, b.StateMask)
	written += writeBEU16(buf, b.TreeMask)
	written += writeBEU16(buf, b.HashMask)
	// root_hash comes BEFORE child hashes (matches Rust to_compact, line 65-68).
	if b.RootHash != nil {
		written += copy(bufWrite(buf, 32), b.RootHash[:])
	}
	for _, h := range b.Hashes {
		written += copy(bufWrite(buf, 32), h[:])
	}
	return written
}

func (b *BranchNodeCompact) DecodeCompact(data []byte, totalLen int) int {
	if totalLen < 6 {
		panic("BranchNodeCompact: totalLen < 6 (masks truncated)")
	}
	cursor := 0

	b.StateMask = readBEU16(data[cursor:])
	cursor += 2
	b.TreeMask = readBEU16(data[cursor:])
	cursor += 2
	b.HashMask = readBEU16(data[cursor:])
	cursor += 2

	// Determine how many 32-byte hash slots remain.
	remaining := totalLen - cursor
	if remaining%32 != 0 {
		panic("BranchNodeCompact: non-multiple-of-32 hash bytes")
	}
	numHashes := remaining / 32
	count := popcount16(b.HashMask)

	// If numHashes == count+1 the first slot is the root_hash.
	b.RootHash = nil
	if numHashes == count+1 {
		var h common.Hash
		copy(h[:], data[cursor:cursor+32])
		b.RootHash = &h
		cursor += 32
		numHashes--
	}
	if numHashes != count {
		panic("BranchNodeCompact: hash count mismatch")
	}

	b.Hashes = make([]common.Hash, count)
	for i := 0; i < count; i++ {
		copy(b.Hashes[i][:], data[cursor:cursor+32])
		cursor += 32
	}

	if cursor != totalLen {
		panic("BranchNodeCompact: cursor != totalLen")
	}
	return cursor
}

func popcount16(v uint16) int {
	v = v - ((v >> 1) & 0x5555)
	v = (v & 0x3333) + ((v >> 2) & 0x3333)
	v = (v + (v >> 4)) & 0x0f0f
	return int((v * 0x0101) >> 8)
}

func writeBEU16(buf *bytes.Buffer, v uint16) int {
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
	return 2
}

func readBEU16(b []byte) uint16 {
	return uint16(b[0])<<8 | uint16(b[1])
}

// StorageTrieEntry is the DupSort value of StoragesTrie. Under v2 we use the
// 33-byte packed SubKey form via EncodePackedCompact; reth's
// PackedStorageTrieEntry::from_compact decodes it (storage.rs).
type StorageTrieEntry struct {
	SubKey StoredNibblesSubKey
	Node   BranchNodeCompact
}

// EncodePackedCompact writes the v2 StoragesTrie DupSort value layout: 33-byte
// PackedStoredNibblesSubKey (32 packed nibble pairs + 1 nibble-count byte)
// followed by the BranchNodeCompact bytes.
//
// SAFE to use with storage_v2=true only — see EncodePackedAccountKey for the
// gating rationale.
func (e *StorageTrieEntry) EncodePackedCompact(buf *bytes.Buffer) int {
	if int(e.SubKey.Length) > 64 {
		panic("StorageTrieEntry: SubKey.Length > 64")
	}
	var out [33]byte
	n := int(e.SubKey.Length)
	pairs := n / 2
	for i := 0; i < pairs; i++ {
		out[i] = (e.SubKey.Nibbles[2*i] << 4) | e.SubKey.Nibbles[2*i+1]
	}
	if n%2 == 1 {
		out[pairs] = e.SubKey.Nibbles[2*pairs] << 4
	}
	out[32] = e.SubKey.Length
	buf.Write(out[:])
	written := 33
	written += e.Node.EncodeCompact(buf)
	return written
}
