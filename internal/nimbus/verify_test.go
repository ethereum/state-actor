package nimbus

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/holiman/uint256"
)

// verify_test.go pins that VerifyState rejects the layouts nimbus would
// misread, using a healthy state mutated one row at a time.

func healthyState(t *testing.T) builtState {
	t.Helper()
	rng := rand.New(rand.NewSource(21))
	accounts := randomAccounts(rng, 120)
	accounts[0].slots = append(accounts[0].slots, testSlot{randomHash(rng), randomValue(rng)})
	accounts[0].balance = uint256.NewInt(1)
	bs := buildState(t, accounts)
	verifyBuilt(t, bs)
	return bs
}

func cloneRows(rows VertexRows) VertexRows {
	out := make(VertexRows, len(rows))
	for k, v := range rows {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func firstOf(bs builtState, pred func(RootedVertexID, Vertex) bool) (RootedVertexID, Vertex) {
	for rvid, rec := range bs.rows {
		v, _ := DecodeVertex(rec)
		if pred(rvid, v) {
			return rvid, v
		}
	}
	panic("no matching vertex")
}

func expectVerifyError(t *testing.T, rows VertexRows, admin SavedState, bs builtState, want string) {
	t.Helper()
	_, err := VerifyState(rows, admin, bs.root)
	if err == nil {
		t.Fatalf("expected an error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func TestVerifyRejectsCorruptKey(t *testing.T) {
	bs := healthyState(t)
	rows := cloneRows(bs.rows)
	rvid, v := firstOf(bs, func(_ RootedVertexID, v Vertex) bool { return v.Type == Branch && v.Key != nil })
	v.Key[0] ^= 0xff
	rows[rvid] = v.Encode()
	expectVerifyError(t, rows, bs.admin, bs, "stored key")
}

func TestVerifyRejectsMissingKey(t *testing.T) {
	bs := healthyState(t)
	rows := cloneRows(bs.rows)
	rvid, v := firstOf(bs, func(r RootedVertexID, v Vertex) bool { return r.Vid != r.Root && v.Type == Branch && v.Key != nil })
	v.Key = nil
	rows[rvid] = v.Encode()
	expectVerifyError(t, rows, bs.admin, bs, "stores no Merkle key")
}

func TestVerifyRejectsOrphanAndMissingRows(t *testing.T) {
	bs := healthyState(t)
	rows := cloneRows(bs.rows)
	rows[RootedVertexID{Root: StateRootVID, Vid: 0x7777}] = rows[RootedVertexID{StateRootVID, StateRootVID}]
	expectVerifyError(t, rows, bs.admin, bs, "unreachable")

	rows = cloneRows(bs.rows)
	rvid, _ := firstOf(bs, func(r RootedVertexID, v Vertex) bool { return v.Type == StoLeaf })
	delete(rows, rvid)
	expectVerifyError(t, rows, bs.admin, bs, "missing vertex")
}

func TestVerifyRejectsMisplacedVertex(t *testing.T) {
	bs := healthyState(t)
	rows := cloneRows(bs.rows)
	// Point a branch's children at a foreign static block: the children then
	// sit at IDs that are not StaticVid(path, pos).
	rvid, v := firstOf(bs, func(r RootedVertexID, v Vertex) bool {
		return r.Root == StateRootVID && v.Type == Branch && v.StartVid < FirstDynamicVID && r.Vid == r.Root
	})
	old := v.StartVid
	v.StartVid = old + 16
	rows[rvid] = v.Encode()
	for n := 0; n < 16; n++ {
		if v.Used&(1<<n) != 0 {
			rows[RootedVertexID{StateRootVID, old + 16 + VertexID(n)}] = rows[RootedVertexID{StateRootVID, old + VertexID(n)}]
			delete(rows, RootedVertexID{StateRootVID, old + VertexID(n)})
		}
	}
	expectVerifyError(t, rows, bs.admin, bs, "static ID")
}

func TestVerifyRejectsBadAdminAndHint(t *testing.T) {
	bs := healthyState(t)
	expectVerifyError(t, bs.rows, SavedState{VTop: uint64(FirstDynamicVID) - 1}, bs, "vTop")

	rows := cloneRows(bs.rows)
	rvid, v := firstOf(bs, func(_ RootedVertexID, v Vertex) bool { return v.Type == AccLeaf && v.StoID != 0 })
	v.StoHint = 0
	rows[rvid] = v.Encode()
	expectVerifyError(t, rows, bs.admin, bs, "stoHint")
}

func TestVerifyRejectsWrongRoot(t *testing.T) {
	bs := healthyState(t)
	want := bs.root
	want[0] ^= 1
	if _, err := VerifyState(bs.rows, bs.admin, want); err == nil || !strings.Contains(err.Error(), "root mismatch") {
		t.Fatalf("err = %v", err)
	}
}
