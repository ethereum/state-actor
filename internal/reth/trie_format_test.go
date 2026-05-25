package reth

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestStoredNibblesRoundtrip(t *testing.T) {
	cases := [][]byte{
		{},                             // empty path
		{0xa},                          // single nibble
		{0xa, 0xb, 0xc},                // odd
		{0xa, 0xb, 0xc, 0xd},           // even
		{0x0, 0x1, 0x2, 0x3, 0x4, 0x5}, // even, mixed
	}
	// 64-nibble (32-byte) max
	full := make([]byte, 64)
	for i := range full {
		full[i] = byte(i % 16)
	}
	cases = append(cases, full)

	for i, nibbles := range cases {
		sn := StoredNibbles{Length: byte(len(nibbles))}
		copy(sn.Nibbles[:], nibbles)
		var buf bytes.Buffer
		sn.EncodeKey(&buf)
		if buf.Len() != 65 {
			t.Fatalf("case %d: encoded len=%d, want 65", i, buf.Len())
		}
		var out StoredNibbles
		out.DecodeKey(buf.Bytes())
		if out.Length != sn.Length {
			t.Errorf("case %d: length %d -> %d", i, sn.Length, out.Length)
		}
		if out.Nibbles != sn.Nibbles {
			t.Errorf("case %d: nibbles mismatch hex=%x", i, buf.Bytes())
		}
	}
}

func TestBranchNodeCompactRoundtrip(t *testing.T) {
	h1 := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	h2 := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	cases := []BranchNodeCompact{
		// minimal: no children
		{StateMask: 0, TreeMask: 0, HashMask: 0, Hashes: nil, RootHash: nil},
		// one hashed child
		{StateMask: 0x0001, TreeMask: 0, HashMask: 0x0001, Hashes: []common.Hash{h1}, RootHash: nil},
		// two hashed children + root
		{StateMask: 0x0003, TreeMask: 0x0002, HashMask: 0x0003, Hashes: []common.Hash{h1, h2}, RootHash: &h1},
		// full state, all hashed
		{
			StateMask: 0xffff, TreeMask: 0x0000, HashMask: 0xffff,
			Hashes:   []common.Hash{h1, h2, h1, h2, h1, h2, h1, h2, h1, h2, h1, h2, h1, h2, h1, h2},
			RootHash: &h2,
		},
	}
	for i, in := range cases {
		var buf bytes.Buffer
		n := in.EncodeCompact(&buf)
		var out BranchNodeCompact
		consumed := out.DecodeCompact(buf.Bytes(), n)
		if consumed != n {
			t.Errorf("case %d: consumed %d, encoded %d", i, consumed, n)
		}
		if !branchNodeEqual(in, out) {
			t.Errorf("case %d: in=%+v out=%+v hex=%x", i, in, out, buf.Bytes())
		}
	}
}

func branchNodeEqual(a, b BranchNodeCompact) bool {
	if a.StateMask != b.StateMask || a.TreeMask != b.TreeMask || a.HashMask != b.HashMask {
		return false
	}
	if len(a.Hashes) != len(b.Hashes) {
		return false
	}
	for i := range a.Hashes {
		if a.Hashes[i] != b.Hashes[i] {
			return false
		}
	}
	if (a.RootHash == nil) != (b.RootHash == nil) {
		return false
	}
	if a.RootHash != nil && *a.RootHash != *b.RootHash {
		return false
	}
	return true
}

// TestEncodePackedAccountKeyShape pins the 33-byte fixed-size layout reth's
// PackedAccountsTrie expects. Byte 32 is the nibble count; bytes 0..32 carry
// packed nibble pairs (high nibble in upper 4 bits, low nibble in lower 4
// bits) right-padded with zeros after the actual nibble count.
//
// Ground truth hand-derived from reth-trie-common
// PackedStoredNibbles::to_compact_array (nibbles.rs:178-189). Any
// future change to the encoder must update these golden bytes
// in lock-step or reth's v2 reader will misdecode.
func TestEncodePackedAccountKeyShape(t *testing.T) {
	type tc struct {
		name    string
		nibbles []byte
		want    [33]byte
	}
	cases := []tc{
		{
			name:    "empty",
			nibbles: []byte{},
			want:    [33]byte{}, // 32 zeros + count=0
		},
		{
			name:    "single nibble (odd)",
			nibbles: []byte{0xA},
			want:    [33]byte{0xA0, 32: 1}, // 0xA in high nibble, rest zero; count=1
		},
		{
			name:    "two nibbles (even)",
			nibbles: []byte{0xA, 0xB},
			want:    [33]byte{0xAB, 32: 2}, // packed; count=2
		},
		{
			name:    "three nibbles (odd, two pairs needed)",
			nibbles: []byte{0x1, 0x2, 0x3},
			want:    [33]byte{0x12, 0x30, 32: 3},
		},
		{
			name:    "four nibbles (even)",
			nibbles: []byte{0xA, 0xB, 0xC, 0xD},
			want:    [33]byte{0xAB, 0xCD, 32: 4},
		},
	}
	// 64 nibbles → fills all 32 packed bytes, count=64
	full := tc{name: "max (64 nibbles)", nibbles: make([]byte, 64)}
	for i := range full.nibbles {
		full.nibbles[i] = byte(i % 16) // 0,1,2,..,F,0,1,2,..
	}
	// Hand-pack: each pair (2i,2i+1) → (2i<<4)|(2i+1) % 16
	for i := 0; i < 32; i++ {
		full.want[i] = (full.nibbles[2*i] << 4) | full.nibbles[2*i+1]
	}
	full.want[32] = 64
	cases = append(cases, full)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sn := StoredNibbles{Length: byte(len(c.nibbles))}
			copy(sn.Nibbles[:], c.nibbles)
			var buf bytes.Buffer
			sn.EncodePackedAccountKey(&buf)
			if buf.Len() != 33 {
				t.Fatalf("packed key = %d bytes, want 33", buf.Len())
			}
			if !bytes.Equal(buf.Bytes(), c.want[:]) {
				t.Errorf("packed key = %x\n  want      %x", buf.Bytes(), c.want[:])
			}
		})
	}
}

// TestEncodePackedCompactShape pins the StoragesTrie DupSort value layout
// under storage_v2: 33-byte PackedStoredNibblesSubKey followed by
// BranchNodeCompact bytes. The packing is reimplemented inline in
// EncodePackedCompact (does not delegate to EncodePackedAccountKey) so
// both encoders need parallel coverage to catch drift between them.
func TestEncodePackedCompactShape(t *testing.T) {
	h := common.HexToHash("0xdeadbeef")
	node := BranchNodeCompact{
		StateMask: 0x0001, TreeMask: 0, HashMask: 0x0001,
		Hashes: []common.Hash{h}, RootHash: nil,
	}
	cases := []struct {
		name        string
		nibbles     []byte
		wantSubKey  [33]byte
	}{
		{"empty", []byte{}, [33]byte{}}, // 32 zeros + count=0
		{"single (odd)", []byte{0xA}, [33]byte{0xA0, 32: 1}},
		{"two (even)", []byte{0xA, 0xB}, [33]byte{0xAB, 32: 2}},
		{"three (odd, two pairs)", []byte{0x1, 0x2, 0x3}, [33]byte{0x12, 0x30, 32: 3}},
		{"four (even)", []byte{0xA, 0xB, 0xC, 0xD}, [33]byte{0xAB, 0xCD, 32: 4}},
	}
	// max 64 nibbles → fills all 32 packed bytes, count=64.
	full := struct {
		name       string
		nibbles    []byte
		wantSubKey [33]byte
	}{name: "max (64)", nibbles: make([]byte, 64)}
	for i := range full.nibbles {
		full.nibbles[i] = byte(i % 16)
	}
	for i := 0; i < 32; i++ {
		full.wantSubKey[i] = (full.nibbles[2*i] << 4) | full.nibbles[2*i+1]
	}
	full.wantSubKey[32] = 64
	cases = append(cases, full)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := StorageTrieEntry{
				SubKey: StoredNibblesSubKey{Length: byte(len(c.nibbles))},
				Node:   node,
			}
			copy(in.SubKey.Nibbles[:], c.nibbles)
			var buf bytes.Buffer
			n := in.EncodePackedCompact(&buf)
			if n < 33 {
				t.Fatalf("encoded %d bytes, want >= 33", n)
			}
			if !bytes.Equal(buf.Bytes()[:33], c.wantSubKey[:]) {
				t.Errorf("packed subkey = %x\n  want         %x", buf.Bytes()[:33], c.wantSubKey[:])
			}
			// BNC payload: 6-byte mask header + 32-byte hash.
			if n-33 != 6+32 {
				t.Errorf("BNC payload = %d bytes, want %d", n-33, 6+32)
			}
		})
	}
}
