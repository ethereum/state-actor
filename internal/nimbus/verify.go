package nimbus

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/ethrex"
)

// VertexRows is every AriVtx row except the admin record, decoded-key form.
type VertexRows map[RootedVertexID][]byte

// CollectRow files one raw AriVtx (key, value) pair: the empty key into admin,
// everything else into rows (the value is copied).
func CollectRow(rows VertexRows, admin *SavedState, key, val []byte) error {
	if len(key) == 0 {
		s, err := DecodeSavedState(val)
		if err != nil {
			return err
		}
		*admin = s
		return nil
	}
	rvid, err := ParseKey(key)
	if err != nil {
		return fmt.Errorf("row key %x: %w", key, err)
	}
	if _, dup := rows[rvid]; dup {
		return fmt.Errorf("duplicate row for %+v", rvid)
	}
	rows[rvid] = append([]byte(nil), val...)
	return nil
}

// VerifyReport summarises what VerifyState walked.
type VerifyReport struct {
	Accounts        int
	StorageTries    int
	StorageLeaves   int
	Branches        int
	ExtBranches     int
	DynamicVertices int // vertices with an ID >= FirstDynamicVID (storage roots + deep nodes)
	MaxDynamicVid   VertexID
}

// VerifyState checks a complete AriVtx row set the way nimbus will read it:
//
//   - every record decodes and is at most MaxVertexBlobSize;
//   - re-deriving every Merkle key bottom-up (ignoring stored keys) reproduces
//     each stored branch key, stores a key exactly when the referenced RLP is
//     >= 32 bytes, and yields wantRoot for the account trie (1,1);
//   - every row is reachable from a root exactly once (no orphans, no sharing);
//   - vertex placement follows the static/dynamic ID scheme: position 0 is the
//     trie root, positions 1..StaticVIDLevels sit at StaticVid(path, pos) with
//     StaticStartVid children, deeper positions and storage roots use dynamic
//     IDs, and admin.VTop covers every dynamic ID;
//   - every leaf resolves both by walking from its root and by the static
//     probe retrieveStatic performs from min(pos, StaticVIDLevels) down to 0;
//   - each stoID is unique, >= FirstDynamicVID, referenced by one AccLeaf whose
//     stoHint is in 1..MaxStoHint (and 0 when there is no storage).
func VerifyState(rows VertexRows, admin SavedState, wantRoot common.Hash) (*VerifyReport, error) {
	v := &verifier{
		vtx:     make(map[RootedVertexID]Vertex, len(rows)),
		visited: make(map[RootedVertexID]struct{}, len(rows)),
		stoIDs:  make(map[VertexID]struct{}),
	}
	for rvid, rec := range rows {
		vx, err := DecodeVertex(rec)
		if err != nil {
			return nil, fmt.Errorf("vertex %+v: %w", rvid, err)
		}
		v.vtx[rvid] = vx
	}
	rootID := RootedVertexID{Root: StateRootVID, Vid: StateRootVID}
	if _, ok := v.vtx[rootID]; !ok {
		return nil, errors.New("missing account trie root vertex (1,1)")
	}
	ref, err := v.hashNode(StateRootVID, StateRootVID, 0, nil)
	if err != nil {
		return nil, err
	}
	if got := crypto.Keccak256Hash(ref); got != wantRoot {
		return nil, fmt.Errorf("account trie root mismatch: got %s want %s", got.Hex(), wantRoot.Hex())
	}
	if len(v.visited) != len(v.vtx) {
		var orphans []string
		for rvid := range v.vtx {
			if _, ok := v.visited[rvid]; !ok {
				orphans = append(orphans, fmt.Sprintf("(%d,%d)", rvid.Root, rvid.Vid))
			}
		}
		sort.Strings(orphans)
		if len(orphans) > 8 {
			orphans = append(orphans[:8], "…")
		}
		return nil, fmt.Errorf("%d unreachable vertex rows: %v", len(v.vtx)-len(v.visited), orphans)
	}
	if v.report.DynamicVertices > 0 && admin.VTop < uint64(v.report.MaxDynamicVid) {
		return nil, fmt.Errorf("admin vTop %d is below the highest dynamic vertex ID %d", admin.VTop, v.report.MaxDynamicVid)
	}
	if v.report.DynamicVertices == 0 && admin.VTop != 0 && admin.VTop < uint64(FirstDynamicVID)-1 {
		return nil, fmt.Errorf("admin vTop %d lies inside the static ID range", admin.VTop)
	}
	for _, lf := range v.leaves {
		if err := v.lookup(lf); err != nil {
			return nil, err
		}
	}
	return &v.report, nil
}

type leafRef struct {
	root VertexID
	key  []byte
	pos  int
}

type verifier struct {
	vtx     map[RootedVertexID]Vertex
	visited map[RootedVertexID]struct{}
	stoIDs  map[VertexID]struct{}
	leaves  []leafRef
	report  VerifyReport
}

// hashNode re-derives the RLP the parent references for the vertex at
// (root, vid), which sits at position pos after consuming path.
func (v *verifier) hashNode(root, vid VertexID, pos int, path []byte) ([]byte, error) {
	rvid := RootedVertexID{Root: root, Vid: vid}
	vx, ok := v.vtx[rvid]
	if !ok {
		return nil, fmt.Errorf("missing vertex (%d,%d) at position %d, path %x", root, vid, pos, path)
	}
	if _, dup := v.visited[rvid]; dup {
		return nil, fmt.Errorf("vertex (%d,%d) is referenced twice", root, vid)
	}
	v.visited[rvid] = struct{}{}

	switch {
	case pos == 0:
		if vid != root {
			return nil, fmt.Errorf("vertex (%d,%d) at position 0 is not the trie root", root, vid)
		}
	case pos <= StaticVIDLevels:
		if want := StaticVid(path, pos); vid != want {
			return nil, fmt.Errorf("vertex (%d,%d) at position %d path %x must have static ID %d", root, vid, pos, path, want)
		}
	default:
		if vid < FirstDynamicVID {
			return nil, fmt.Errorf("vertex (%d,%d) at position %d must have a dynamic ID", root, vid, pos)
		}
	}
	if vid >= FirstDynamicVID {
		v.report.DynamicVertices++
		if vid > v.report.MaxDynamicVid {
			v.report.MaxDynamicVid = vid
		}
	}

	switch vx.Type {
	case Branch, ExtBranch:
		full := make([]byte, 0, len(path)+len(vx.Pfx)+1)
		full = append(full, path...)
		full = append(full, vx.Pfx...)
		childPos := len(full) + 1
		if childPos <= StaticVIDLevels {
			if want := StaticStartVid(full, childPos); vx.StartVid != want {
				return nil, fmt.Errorf("branch (%d,%d) children at position %d must start at static ID %d, have %d", root, vid, childPos, want, vx.StartVid)
			}
		} else if vx.StartVid < FirstDynamicVID {
			return nil, fmt.Errorf("branch (%d,%d) children at position %d must start at a dynamic ID, have %d", root, vid, childPos, vx.StartVid)
		}
		if bits.OnesCount16(vx.Used) < 2 {
			return nil, fmt.Errorf("branch (%d,%d) has %d children", root, vid, bits.OnesCount16(vx.Used))
		}
		var children [16][]byte
		for n := 0; n < 16; n++ {
			if vx.Used&(1<<n) == 0 {
				continue
			}
			childPath := append(full[:len(full):len(full)], byte(n))
			ref, err := v.hashNode(root, vx.StartVid+VertexID(n), childPos, childPath)
			if err != nil {
				return nil, err
			}
			children[n] = ref
		}
		branchRLP := ethrex.EncodeBranch(children, nil)
		ref := branchRLP
		if vx.Type == ExtBranch {
			ref = ethrex.EncodeExtension(vx.Pfx, branchRLP)
			v.report.ExtBranches++
		} else {
			v.report.Branches++
		}
		if len(ref) >= 32 {
			want := crypto.Keccak256(ref)
			if vx.Key == nil {
				return nil, fmt.Errorf("branch (%d,%d) stores no Merkle key", root, vid)
			}
			if !bytes.Equal(vx.Key, want) {
				return nil, fmt.Errorf("branch (%d,%d) stored key %x, re-derived %x", root, vid, vx.Key, want)
			}
		} else if vx.Key != nil {
			return nil, fmt.Errorf("branch (%d,%d) stores a key for a %d-byte (inline) node", root, vid, len(ref))
		}
		return ref, nil

	case AccLeaf:
		if root != StateRootVID {
			return nil, fmt.Errorf("account leaf (%d,%d) inside a storage trie", root, vid)
		}
		key := append(append(make([]byte, 0, 64), path...), vx.Pfx...)
		if len(key) != 64 {
			return nil, fmt.Errorf("account leaf (%d,%d) path length %d", root, vid, len(key))
		}
		storageRoot := EmptyRootHash
		if vx.StoID != 0 {
			if vx.StoID < FirstDynamicVID {
				return nil, fmt.Errorf("account leaf (%d,%d) stoID %d is not dynamic", root, vid, vx.StoID)
			}
			if _, dup := v.stoIDs[vx.StoID]; dup {
				return nil, fmt.Errorf("stoID %d referenced by more than one account", vx.StoID)
			}
			v.stoIDs[vx.StoID] = struct{}{}
			if vx.StoHint < 1 || vx.StoHint > MaxStoHint {
				return nil, fmt.Errorf("account leaf (%d,%d) stoHint %d outside 1..%d", root, vid, vx.StoHint, MaxStoHint)
			}
			if _, ok := v.vtx[RootedVertexID{Root: vx.StoID, Vid: vx.StoID}]; !ok {
				return nil, fmt.Errorf("account leaf (%d,%d) references missing storage root (%d,%d)", root, vid, vx.StoID, vx.StoID)
			}
			stRef, err := v.hashNode(vx.StoID, vx.StoID, 0, nil)
			if err != nil {
				return nil, err
			}
			storageRoot = crypto.Keccak256Hash(stRef)
			v.report.StorageTries++
		} else if vx.StoHint != 0 {
			return nil, fmt.Errorf("account leaf (%d,%d) has stoHint %d without storage", root, vid, vx.StoHint)
		}
		v.leaves = append(v.leaves, leafRef{root: root, key: key, pos: pos})
		v.report.Accounts++
		valueRLP := ethrex.EncodeAccountState(vx.Nonce, vx.Balance, storageRoot, vx.CodeHash)
		return ethrex.EncodeLeaf(vx.Pfx, valueRLP), nil

	case StoLeaf:
		if root == StateRootVID {
			return nil, fmt.Errorf("storage leaf (%d,%d) inside the account trie", root, vid)
		}
		key := append(append(make([]byte, 0, 64), path...), vx.Pfx...)
		if len(key) != 64 {
			return nil, fmt.Errorf("storage leaf (%d,%d) path length %d", root, vid, len(key))
		}
		if vx.StoValue == (common.Hash{}) {
			return nil, fmt.Errorf("storage leaf (%d,%d) holds a zero value", root, vid)
		}
		v.leaves = append(v.leaves, leafRef{root: root, key: key, pos: pos})
		v.report.StorageLeaves++
		return ethrex.EncodeLeaf(vx.Pfx, ethrex.EncodeStorageValueBytes32(vx.StoValue)), nil
	}
	return nil, fmt.Errorf("vertex (%d,%d): unknown type %v", root, vid, vx.Type)
}

// lookup resolves one leaf the two ways nimbus does: walking from the root
// and probing static IDs from min(pos, StaticVIDLevels) down to 0.
func (v *verifier) lookup(lf leafRef) error {
	root, key := lf.root, lf.key
	walkVid := make(map[int]VertexID, 16)
	pos := 0
	vid := root
	for {
		vx, ok := v.vtx[RootedVertexID{Root: root, Vid: vid}]
		if !ok {
			return fmt.Errorf("walk for key %x in trie %d: missing vertex %d at position %d", key, root, vid, pos)
		}
		walkVid[pos] = vid
		switch vx.Type {
		case Branch, ExtBranch:
			if vx.Type == ExtBranch {
				if pos+len(vx.Pfx) > len(key) || !bytes.Equal(key[pos:pos+len(vx.Pfx)], vx.Pfx) {
					return fmt.Errorf("walk for key %x in trie %d: extension prefix mismatch at position %d", key, root, pos)
				}
				pos += len(vx.Pfx)
			}
			if pos >= len(key) {
				return fmt.Errorf("walk for key %x in trie %d: ran out of nibbles at a branch", key, root)
			}
			nib := key[pos]
			if vx.Used&(1<<nib) == 0 {
				return fmt.Errorf("walk for key %x in trie %d: branch at position %d lacks child %x", key, root, pos, nib)
			}
			vid = vx.StartVid + VertexID(nib)
			pos++
		case AccLeaf, StoLeaf:
			if !bytes.Equal(vx.Pfx, key[pos:]) {
				return fmt.Errorf("walk for key %x in trie %d: leaf at position %d has prefix %x", key, root, pos, vx.Pfx)
			}
			if pos != lf.pos {
				return fmt.Errorf("walk for key %x in trie %d: leaf found at position %d, hashed at %d", key, root, pos, lf.pos)
			}
			return v.probe(root, key, lf.pos, walkVid)
		}
	}
}

// probe mirrors aristo_fetch.nim retrieveStatic: for each static level from
// the leaf's level down to 0, a row at (root, StaticVid(key, level)) must be
// exactly the vertex the walk passed at that position — otherwise the reader
// would trust a vertex from another path and report the leaf missing.
func (v *verifier) probe(root VertexID, key []byte, leafPos int, walkVid map[int]VertexID) error {
	top := leafPos
	if top > StaticVIDLevels {
		top = StaticVIDLevels
	}
	for sl := top; sl >= 0; sl-- {
		svid := root
		if sl > 0 {
			svid = StaticVid(key, sl)
		}
		_, exists := v.vtx[RootedVertexID{Root: root, Vid: svid}]
		wv, onPath := walkVid[sl]
		switch {
		case exists && (!onPath || wv != svid):
			return fmt.Errorf("static probe for key %x in trie %d at level %d hits vertex %d, which is not on the key's path", key, root, sl, svid)
		case !exists && onPath:
			return fmt.Errorf("static probe for key %x in trie %d at level %d misses vertex %d that the walk visited", key, root, sl, wv)
		}
	}
	return nil
}
