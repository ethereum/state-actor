package nimbus

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// RootedVertexID addresses one vertex: the trie it belongs to (Root — 1 for
// the account trie, the account's stoID for a storage trie) and its own ID.
type RootedVertexID struct {
	Root VertexID
	Vid  VertexID
}

// StaticVid is aristo_vid.nim's staticVid: the ID of the vertex reached after
// consuming path[:level] nibbles, for 0 <= level <= StaticVIDLevels. Level 0
// is FirstStaticVID for every trie (the reader substitutes the trie root
// itself at level 0, so a storage trie's root row is keyed (stoID, stoID)).
func StaticVid(path []byte, level int) VertexID {
	if level == 0 {
		return FirstStaticVID
	}
	v := uint64(FirstStaticVID)
	for i := 0; i < level; i++ {
		v += 1 << (uint(i) * 4)
		v += uint64(path[i]) << (uint(level-i-1) * 4)
	}
	return VertexID(v)
}

// StaticStartVid is the startVid of a branch whose children sit at position
// childPos (1..StaticVIDLevels) under path: StaticVid(path, childPos) ==
// StaticStartVid(path, childPos) + path[childPos-1]. Only path[:childPos-1] is
// read, so any key inside the branch's subtree may be passed. Mirrors
// aristo_merge.nim's staticVidFetch(path.slice(0, pos+n) & nibble(0), 16).
func StaticStartVid(path []byte, childPos int) VertexID {
	v := StaticOffset[childPos]
	for i := 0; i < childPos-1; i++ {
		v += uint64(path[i]) << (uint(childPos-1-i) * 4)
	}
	return VertexID(v)
}

// IsStatic reports whether v lies in the static ID range [1, FirstDynamicVID).
func IsStatic(v VertexID) bool {
	return v >= FirstStaticVID && v < FirstDynamicVID
}

// AppendKey appends the AriVtx row key for r: [len(SBE(root))] ‖ SBE(root) ‖
// SBE(vid), with the SBE(vid) part omitted when vid == root
// (aristo_blobify.nim blobify(RootedVertexID)).
func AppendKey(dst []byte, r RootedVertexID) []byte {
	dst = append(dst, byte(SBELen(uint64(r.Root))))
	dst = AppendSBE(dst, uint64(r.Root))
	if r.Vid != r.Root {
		dst = AppendSBE(dst, uint64(r.Vid))
	}
	return dst
}

var errKey = errors.New("nimbus: malformed RootedVertexID key")

// ParseKey reverses AppendKey.
func ParseKey(b []byte) (RootedVertexID, error) {
	if len(b) < 2 {
		return RootedVertexID{}, errKey
	}
	rlen := int(b[0])
	if rlen < 1 || rlen > 8 || len(b) < rlen+1 {
		return RootedVertexID{}, fmt.Errorf("%w: root length %d", errKey, rlen)
	}
	root, err := DecodeSBE(b[1 : rlen+1])
	if err != nil {
		return RootedVertexID{}, err
	}
	vid := root
	if len(b) > rlen+1 {
		vid, err = DecodeSBE(b[rlen+1:])
		if err != nil {
			return RootedVertexID{}, err
		}
		if vid == root {
			return RootedVertexID{}, fmt.Errorf("%w: vid == root must be omitted", errKey)
		}
	}
	return RootedVertexID{Root: VertexID(root), Vid: VertexID(vid)}, nil
}

// VidAllocator is the process-wide dynamic vertex-ID counter, mirroring
// AristoTxRef.vidFetch: the first allocation starts at FirstDynamicVID and
// every allocation returns a contiguous block. It is safe for concurrent use;
// block numbering therefore depends on goroutine timing, which is fine —
// vertex IDs are never hashed and every ID is above the static range.
type VidAllocator struct {
	top     atomic.Uint64
	touched atomic.Bool
}

// NewVidAllocator returns an allocator positioned just below FirstDynamicVID.
func NewVidAllocator() *VidAllocator {
	a := &VidAllocator{}
	a.top.Store(uint64(FirstDynamicVID) - 1)
	return a
}

// Alloc reserves n consecutive IDs and returns the first.
func (a *VidAllocator) Alloc(n uint64) VertexID {
	a.touched.Store(true)
	return VertexID(a.top.Add(n) - n + 1)
}

// VTop is the value to persist as SavedState.vTop: the highest ID handed out,
// or 0 when nothing was allocated (nimbus lazily seeds the counter from 0).
func (a *VidAllocator) VTop() uint64 {
	if !a.touched.Load() {
		return 0
	}
	return a.top.Load()
}
