package storage

import (
	"bytes"
	"testing"

	"github.com/ethereum/state-actor/internal/neth"
)

// TestStateNodeKey_LayoutShallowPath pins the byte layout for a path
// length ≤ TopStateBoundary (section byte = 0).
func TestStateNodeKey_LayoutShallowPath(t *testing.T) {
	path := [32]byte{}
	path[0] = 0xab // first byte of the byte-packed path
	path[1] = 0xcd
	path[7] = 0xef
	path[8] = 0xff // ignored — only first 8 bytes count

	keccak := [32]byte{}
	for i := range keccak {
		keccak[i] = byte(0x10 + i)
	}

	got := StateNodeKey(path[:], 5, keccak)

	if len(got) != StateNodeKeyLen {
		t.Fatalf("output length: got %d, want %d", len(got), StateNodeKeyLen)
	}
	if got[0] != 0 {
		t.Errorf("section byte: got 0x%02x, want 0 (pathLen=5 <= TopStateBoundary)", got[0])
	}
	if !bytes.Equal(got[1:9], path[:8]) {
		t.Errorf("path bytes: got %x, want %x", got[1:9], path[:8])
	}
	if got[9] != 5 {
		t.Errorf("pathLen byte: got %d, want 5", got[9])
	}
	if !bytes.Equal(got[10:42], keccak[:]) {
		t.Errorf("keccak bytes: got %x, want %x", got[10:42], keccak[:])
	}
}

// TestStateNodeKey_DeepPath exercises the section-byte boundary: pathLen=6
// (just past the boundary) must produce section byte 1, while pathLen=5
// must still be 0.
func TestStateNodeKey_DeepPath(t *testing.T) {
	path := make([]byte, 8)
	keccak := [32]byte{}

	for _, c := range []struct {
		pathLen     int
		wantSection byte
	}{
		{0, 0},
		{1, 0},
		{neth.TopStateBoundary, 0},     // boundary inclusive
		{neth.TopStateBoundary + 1, 1}, // boundary + 1
		{32, 1},
		{64, 1},
	} {
		got := StateNodeKey(path, c.pathLen, keccak)
		if got[0] != c.wantSection {
			t.Errorf("pathLen=%d: section byte got 0x%02x, want 0x%02x", c.pathLen, got[0], c.wantSection)
		}
		if got[9] != byte(c.pathLen) {
			t.Errorf("pathLen=%d: pathLen byte got %d, want %d", c.pathLen, got[9], c.pathLen)
		}
	}
}

// TestStorageNodeKey_Layout pins the 74-byte layout: section(1) ||
// addrHash(32) || path[:8] || pathLen(1) || keccak(32).
func TestStorageNodeKey_Layout(t *testing.T) {
	var addrHash [32]byte
	for i := range addrHash {
		addrHash[i] = byte(i + 0x40)
	}
	path := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}
	var keccak [32]byte
	for i := range keccak {
		keccak[i] = byte(0xa0 + i)
	}

	got := StorageNodeKey(addrHash, path, 64, keccak)

	if len(got) != StorageNodeKeyLen {
		t.Fatalf("output length: got %d, want %d", len(got), StorageNodeKeyLen)
	}
	if got[0] != 2 {
		t.Errorf("section byte: got 0x%02x, want 2 (storage)", got[0])
	}
	if !bytes.Equal(got[1:33], addrHash[:]) {
		t.Errorf("addrHash bytes: got %x, want %x", got[1:33], addrHash[:])
	}
	if !bytes.Equal(got[33:41], path[:8]) {
		t.Errorf("path bytes: got %x, want %x", got[33:41], path[:8])
	}
	if got[41] != 64 {
		t.Errorf("pathLen byte: got %d, want 64", got[41])
	}
	if !bytes.Equal(got[42:74], keccak[:]) {
		t.Errorf("keccak bytes: got %x, want %x", got[42:74], keccak[:])
	}
}

// TestHashOnlyKey: just a 32-byte copy of the keccak hash. Nethermind's
// fallback encoding when HalfPath is disabled.
func TestHashOnlyKey(t *testing.T) {
	var keccak [32]byte
	for i := range keccak {
		keccak[i] = byte(i)
	}
	got := HashOnlyKey(keccak)
	if len(got) != HashOnlyKeyLen {
		t.Fatalf("output length: got %d, want 32", len(got))
	}
	if !bytes.Equal(got, keccak[:]) {
		t.Errorf("HashOnlyKey: got %x, want %x", got, keccak[:])
	}
}

// TestStateNodeKey_PanicShortPath: under-8-byte path must panic, matching
// the Nethermind precondition that the `TreePath.Path.BytesAsSpan` is at
// least 8 bytes wide.
func TestStateNodeKey_PanicShortPath(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("StateNodeKey did not panic on 4-byte path")
		}
	}()
	StateNodeKey(make([]byte, 4), 1, [32]byte{})
}

// TestStorageNodeKey_PanicShortPath: same precondition for storage keys.
func TestStorageNodeKey_PanicShortPath(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("StorageNodeKey did not panic on 7-byte path")
		}
	}()
	StorageNodeKey([32]byte{}, make([]byte, 7), 1, [32]byte{})
}

// TestKeyEncodingsDistinct verifies the same logical keccak yields three
// DIFFERENT physical keys (state-shallow, state-deep, storage), so prefix
// scans within a single column family don't accidentally hit nodes from
// the wrong section.
func TestKeyEncodingsDistinct(t *testing.T) {
	path := make([]byte, 8)
	var keccak [32]byte
	for i := range keccak {
		keccak[i] = byte(0x55)
	}
	var addrHash [32]byte

	stateShallow := StateNodeKey(path, 4, keccak)
	stateDeep := StateNodeKey(path, 10, keccak)
	storage := StorageNodeKey(addrHash, path, 4, keccak)
	hashOnly := HashOnlyKey(keccak)

	if bytes.Equal(stateShallow, stateDeep) {
		t.Error("state-shallow and state-deep keys must differ in section byte")
	}
	if bytes.Equal(stateShallow, storage) {
		t.Error("state and storage keys must differ in section byte")
	}
	if bytes.Equal(stateDeep, hashOnly) {
		t.Error("HalfPath and Hash-only keys must differ in length")
	}
}
