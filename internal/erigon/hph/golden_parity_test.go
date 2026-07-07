//go:build cgo_erigon_commitment

package hph_test

// Golden A — the vendor-faithfulness oracle. The vendored engine
// (internal/erigon/hph) and the erigon-module engine
// (github.com/erigontech/erigon/execution/commitment) are importable side by
// side in one binary; this test drives BOTH through the identical 16-way
// concurrent Process over the same fixture and asserts the root AND every
// persisted branch row are byte-identical. Upstream only ever asserts
// root-hash equality — the branch-byte comparison is the coverage that makes
// vendoring mechanical rather than hopeful. Any local modification to the
// vendored copy that changes bytes fails here first.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/empty"
	erigonkv "github.com/erigontech/erigon/db/kv"
	upstream "github.com/erigontech/erigon/execution/commitment"

	"github.com/ethereum/state-actor/internal/erigon/hph"
)

// fixture returns n unique 20-byte addresses with nonce/balance, half of
// them carrying two storage slots — spanning all 16 first nibbles of
// keccak(addr) with multi-level branch structure.
type fixAccount struct {
	addr    [20]byte
	nonce   uint64
	balance *uint256.Int
	code    []byte
	slots   [][2][32]byte // key, value
}

func fixture(n int) []fixAccount {
	out := make([]fixAccount, 0, n)
	for i := 0; i < n; i++ {
		var a fixAccount
		binary.BigEndian.PutUint64(a.addr[12:], uint64(i+1))
		a.nonce = uint64(i + 1)
		a.balance = uint256.NewInt(uint64(1000 * (i + 1)))
		if i%2 == 0 {
			for j := 0; j < 2; j++ {
				var k, v [32]byte
				k[0], k[31] = byte(i), byte(j+1)
				v[31] = byte(j + 7)
				a.slots = append(a.slots, [2][32]byte{k, v})
			}
		}
		if i%5 == 0 { // code-bearing: CodeUpdate changes the leaf hash
			a.code = append([]byte{0xef, 0x01, 0x00}, a.addr[:]...)
		}
		out = append(out, a)
	}
	return out
}

// mockState is the in-RAM PatriciaContext state shared by both engine runs:
// branch rows (concurrent-safe) plus the encoded account/storage updates each
// engine re-fetches during the fold. The two engines get separate instances.
type mockState struct {
	mu       sync.Mutex
	branches map[string][]byte
	accounts map[string][]byte // plain addr -> encoded Update
	storage  map[string][]byte // addr||slot -> encoded Update
}

func (m *mockState) branch(prefix []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.branches[string(prefix)], nil
}

func (m *mockState) putBranch(prefix, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.branches[string(prefix)] = append([]byte(nil), data...)
	return nil
}

// vendoredCtx implements hph.PatriciaContext.
type vendoredCtx struct{ s *mockState }

func (c *vendoredCtx) Branch(pfx []byte) ([]byte, erigonkv.Step, error) {
	d, err := c.s.branch(pfx)
	return d, 0, err
}
func (c *vendoredCtx) PutBranch(pfx, data, prev []byte) error { return c.s.putBranch(pfx, data) }
func (c *vendoredCtx) Account(plainKey []byte) (*hph.Update, error) {
	enc := c.s.accounts[string(plainKey)]
	if enc == nil {
		return nil, fmt.Errorf("mock: unknown account %x", plainKey)
	}
	u := new(hph.Update)
	if _, err := u.Decode(enc, 0); err != nil {
		return nil, err
	}
	return u, nil
}
func (c *vendoredCtx) Storage(plainKey []byte) (*hph.Update, error) {
	enc := c.s.storage[string(plainKey)]
	if enc == nil {
		return nil, fmt.Errorf("mock: unknown storage %x", plainKey)
	}
	u := new(hph.Update)
	if _, err := u.Decode(enc, 0); err != nil {
		return nil, err
	}
	return u, nil
}

// upstreamCtx implements upstream commitment.PatriciaContext.
type upstreamCtx struct{ s *mockState }

func (c *upstreamCtx) Branch(pfx []byte) ([]byte, erigonkv.Step, error) {
	d, err := c.s.branch(pfx)
	return d, 0, err
}
func (c *upstreamCtx) PutBranch(pfx, data, prev []byte) error { return c.s.putBranch(pfx, data) }
func (c *upstreamCtx) Account(plainKey []byte) (*upstream.Update, error) {
	enc := c.s.accounts[string(plainKey)]
	if enc == nil {
		return nil, fmt.Errorf("mock: unknown account %x", plainKey)
	}
	u := new(upstream.Update)
	if _, err := u.Decode(enc, 0); err != nil {
		return nil, err
	}
	return u, nil
}
func (c *upstreamCtx) Storage(plainKey []byte) (*upstream.Update, error) {
	enc := c.s.storage[string(plainKey)]
	if enc == nil {
		return nil, fmt.Errorf("mock: unknown storage %x", plainKey)
	}
	u := new(upstream.Update)
	if _, err := u.Decode(enc, 0); err != nil {
		return nil, err
	}
	return u, nil
}

// encodeFixture encodes the fixture's updates ONCE with the given encoder
// funcs (each package's Update.Encode wire format is byte-identical — that is
// itself part of what Golden A pins, since both decoders consume both).
func encodeFixture(accs []fixAccount, s *mockState,
	encAcc func(nonce uint64, bal *uint256.Int, code []byte) []byte,
	encStor func(val []byte) []byte) (plainKeys []string) {
	for _, a := range accs {
		s.accounts[string(a.addr[:])] = encAcc(a.nonce, a.balance, a.code)
		plainKeys = append(plainKeys, string(a.addr[:]))
		for _, kv := range a.slots {
			pk := string(a.addr[:]) + string(kv[0][:])
			s.storage[pk] = encStor(kv[1][:])
			plainKeys = append(plainKeys, pk)
		}
	}
	return plainKeys
}

func encodeVendoredAcc(nonce uint64, bal *uint256.Int, code []byte) []byte {
	u := hph.Update{Flags: hph.NonceUpdate | hph.BalanceUpdate, Nonce: nonce}
	u.Balance = *bal
	u.CodeHash = empty.CodeHash
	if len(code) > 0 {
		u.CodeHash = crypto.Keccak256Hash(code)
		u.Flags |= hph.CodeUpdate
	}
	var nb [binary.MaxVarintLen64]byte
	return u.Encode(nil, nb[:])
}

func encodeVendoredStor(val []byte) []byte {
	i := 0
	for i < len(val) && val[i] == 0 {
		i++
	}
	trimmed := val[i:]
	u := hph.Update{Flags: hph.StorageUpdate, StorageLen: int8(len(trimmed))}
	copy(u.Storage[:], trimmed)
	var nb [binary.MaxVarintLen64]byte
	return u.Encode(nil, nb[:])
}

func encodeUpstreamAcc(nonce uint64, bal *uint256.Int, code []byte) []byte {
	u := upstream.Update{Flags: upstream.NonceUpdate | upstream.BalanceUpdate, Nonce: nonce}
	u.Balance = *bal
	u.CodeHash = empty.CodeHash
	if len(code) > 0 {
		u.CodeHash = crypto.Keccak256Hash(code)
		u.Flags |= upstream.CodeUpdate
	}
	var nb [binary.MaxVarintLen64]byte
	return u.Encode(nil, nb[:])
}

func encodeUpstreamStor(val []byte) []byte {
	i := 0
	for i < len(val) && val[i] == 0 {
		i++
	}
	trimmed := val[i:]
	u := upstream.Update{Flags: upstream.StorageUpdate, StorageLen: int8(len(trimmed))}
	copy(u.Storage[:], trimmed)
	var nb [binary.MaxVarintLen64]byte
	return u.Encode(nil, nb[:])
}

func branchBytes(m map[string][]byte) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	for _, k := range keys {
		buf.WriteString(k)
		buf.Write(m[k])
	}
	return buf.Bytes()
}

// sliceStream serves one first-nibble shard's rows to DirectFold in
// ascending hashedKey order — the in-memory analogue of production's
// cursorStream.
type streamRow struct{ hashedKey, plainKey, enc []byte }

type sliceStream struct {
	rows []streamRow
	i    int
	u    hph.Update // reused; the engine copies what it keeps
}

func (s *sliceStream) Next() (hashedKey, plainKey []byte, u *hph.Update, ok bool, err error) {
	if s.i >= len(s.rows) {
		return nil, nil, nil, false, nil
	}
	r := s.rows[s.i]
	s.i++
	s.u = hph.Update{} // full reset — Decode writes only flagged fields
	if _, e := s.u.Decode(r.enc, 0); e != nil {
		return nil, nil, nil, false, e
	}
	return r.hashedKey, r.plainKey, &s.u, true, nil
}

// TestGoldenA_VendoredMatchesUpstream16Way is the vendor-faithfulness gate:
// the vendored DirectFold (the production path) must be byte-identical to
// the UPSTREAM module engine on root, every branch row, and HPHState.
func TestGoldenA_VendoredMatchesUpstream16Way(t *testing.T) {
	accs := fixture(2048)

	// --- vendored DirectFold run (the production choreography) ---
	vs := &mockState{branches: map[string][]byte{}, accounts: map[string][]byte{}, storage: map[string][]byte{}}
	vKeys := encodeFixture(accs, vs, encodeVendoredAcc, encodeVendoredStor)
	var shards [16][]streamRow
	for _, k := range vKeys {
		pk := []byte(k)
		hk := hph.KeyToHexNibbleHash(pk) // [0] == worker shard (InputPart)
		enc := vs.accounts[k]
		if enc == nil {
			enc = vs.storage[k]
		}
		shards[hk[0]] = append(shards[hk[0]], streamRow{hashedKey: hk, plainKey: pk, enc: enc})
	}
	var streams [16]hph.KeyStream
	for n := range shards {
		sh := shards[n]
		sort.Slice(sh, func(i, j int) bool { return bytes.Compare(sh[i].hashedKey, sh[j].hashedKey) < 0 })
		streams[n] = &sliceStream{rows: sh} // ascending hashed order — the engine's sort order
	}
	vCtx := &vendoredCtx{s: vs}
	vHph := hph.NewHexPatriciaHashed(20, vCtx)
	vPph := hph.NewConcurrentPatriciaHashed(vHph, vCtx)
	factory := func() (hph.PatriciaContext, func()) { return &vendoredCtx{s: vs}, func() {} }
	vRoot, err := hph.DirectFold(context.Background(), vPph, factory, &streams)
	if err != nil {
		t.Fatalf("DirectFold: %v", err)
	}
	if err := vHph.ApplyAndClearInlineDeferredUpdates(); err != nil {
		t.Fatalf("vendored ApplyDeferred: %v", err)
	}

	// --- upstream (erigon module) engine run ---
	us := &mockState{branches: map[string][]byte{}, accounts: map[string][]byte{}, storage: map[string][]byte{}}
	uKeys := encodeFixture(accs, us, encodeUpstreamAcc, encodeUpstreamStor)
	uCtx := &upstreamCtx{s: us}
	uUpds := upstream.NewUpdates(upstream.ModeDirect, t.TempDir(), upstream.KeyToHexNibbleHash)
	uUpds.SetConcurrentCommitment(true)
	var placeholderU upstream.Update
	for _, k := range uKeys {
		uUpds.TouchPlainKeyDirect(k, &placeholderU)
	}
	uHph := upstream.NewHexPatriciaHashed(20, uCtx)
	uPph := upstream.NewConcurrentPatriciaHashed(uHph, uCtx)
	uRoot, err := uPph.Process(context.Background(), uUpds, "goldenA-upstream", nil,
		upstream.WarmupConfig{CtxFactory: func() (upstream.PatriciaContext, func()) { return &upstreamCtx{s: us}, func() {} }})
	if err != nil {
		t.Fatalf("upstream Process: %v", err)
	}
	if err := uHph.ApplyAndClearInlineDeferredUpdates(); err != nil {
		t.Fatalf("upstream ApplyDeferred: %v", err)
	}

	// --- byte comparisons ---
	if !bytes.Equal(vRoot, uRoot) {
		t.Fatalf("ROOT DIVERGED: vendored=%x upstream=%x", vRoot, uRoot)
	}
	// Cross-check the update wire encodings agree (both maps fed both engines
	// with their own encoder — the byte forms must be identical too).
	for k, v := range vs.accounts {
		if !bytes.Equal(v, us.accounts[k]) {
			t.Fatalf("Update account encoding diverged for %x", k)
		}
	}
	if len(vs.branches) != len(us.branches) {
		t.Fatalf("branch row count diverged: vendored=%d upstream=%d", len(vs.branches), len(us.branches))
	}
	vState, verr := vHph.EncodeCurrentState(nil)
	uState, uerr := uHph.EncodeCurrentState(nil)
	if verr != nil || uerr != nil {
		t.Fatalf("EncodeCurrentState: vendored=%v upstream=%v", verr, uerr)
	}
	if !bytes.Equal(vState, uState) {
		t.Fatalf("HPHState DIVERGED: vendored=%x upstream=%x", vState, uState)
	}
	if !bytes.Equal(branchBytes(vs.branches), branchBytes(us.branches)) {
		for k, v := range vs.branches {
			if !bytes.Equal(v, us.branches[k]) {
				t.Fatalf("branch row %x diverged:\n vendored=%x\n upstream=%x", k, v, us.branches[k])
			}
		}
		t.Fatal("branch byte sets diverged (key sets differ)")
	}
	t.Logf("Golden A: root %x, %d branch rows byte-identical across vendored and upstream engines", vRoot, len(vs.branches))
}
