package nimbus

import (
	"bytes"
	"encoding/hex"
	"math"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/ethrex"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func TestStaticOffsetTable(t *testing.T) {
	v := uint64(1)
	for d := 0; d <= StaticVIDLevels+1; d++ {
		if StaticOffset[d] != v {
			t.Fatalf("StaticOffset[%d] = %d, want %d", d, StaticOffset[d], v)
		}
		v += 1 << (4 * uint(d))
	}
	if VertexID(StaticOffset[StaticVIDLevels+1]) != FirstDynamicVID {
		t.Fatalf("StaticOffset[9] = %d != FirstDynamicVID %d", StaticOffset[StaticVIDLevels+1], FirstDynamicVID)
	}
}

func TestStaticVidIdentity(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		var path [64]byte
		for i := range path {
			path[i] = byte(rng.Intn(16))
		}
		if got := StaticVid(path[:], 0); got != FirstStaticVID {
			t.Fatalf("level 0 → %d", got)
		}
		for c := 1; c <= StaticVIDLevels; c++ {
			sv := StaticVid(path[:], c)
			if want := StaticStartVid(path[:], c) + VertexID(path[c-1]); sv != want {
				t.Fatalf("StaticVid(level %d) = %d, StaticStartVid+nibble = %d", c, sv, want)
			}
			if uint64(sv) < StaticOffset[c] || uint64(sv) >= StaticOffset[c+1] {
				t.Fatalf("StaticVid(level %d) = %d outside [%d,%d)", c, sv, StaticOffset[c], StaticOffset[c+1])
			}
			if !IsStatic(sv) {
				t.Fatalf("StaticVid(level %d) = %d not static", c, sv)
			}
		}
	}
	zeros := make([]byte, 64)
	if got := StaticVid(zeros, StaticVIDLevels); uint64(got) != StaticOffset[StaticVIDLevels] {
		t.Fatalf("all-zero path level 8 → %d", got)
	}
	fs := bytes.Repeat([]byte{0xf}, 64)
	if got := StaticVid(fs, StaticVIDLevels); got != FirstDynamicVID-1 {
		t.Fatalf("all-f path level 8 → %d, want %d", got, FirstDynamicVID-1)
	}
	if IsStatic(FirstDynamicVID) || IsStatic(0) {
		t.Fatal("IsStatic range")
	}
}

func TestAppendKeyVectors(t *testing.T) {
	cases := []struct {
		r    RootedVertexID
		want string
	}{
		{RootedVertexID{1, 1}, "0101"},
		{RootedVertexID{1, 0x12}, "010112"},
		{RootedVertexID{FirstDynamicVID, FirstDynamicVID}, "050111111112"},
		{RootedVertexID{FirstDynamicVID, 2}, "05011111111202"},
		{RootedVertexID{1, FirstDynamicVID}, "0101" + "0111111112"},
	}
	for _, c := range cases {
		got := AppendKey(nil, c.r)
		if hex.EncodeToString(got) != c.want {
			t.Fatalf("AppendKey(%+v) = %x, want %s", c.r, got, c.want)
		}
		back, err := ParseKey(got)
		if err != nil || back != c.r {
			t.Fatalf("ParseKey(%x) = %+v, %v", got, back, err)
		}
	}
	if _, err := ParseKey([]byte{0x01, 0x01, 0x01}); err == nil {
		t.Fatal("ParseKey accepted an explicit vid == root")
	}
}

func TestHexPrefixVectors(t *testing.T) {
	cases := []struct {
		nib  []byte
		leaf bool
		want string
	}{
		{[]byte{1, 2, 3, 4, 5}, false, "112345"},
		{[]byte{0, 1, 2, 3, 4, 5}, false, "00012345"},
		{[]byte{0xf, 1, 0xc, 0xb, 8}, true, "3f1cb8"},
		{[]byte{0, 0xf, 1, 0xc, 0xb, 8}, true, "200f1cb8"},
		{nil, true, "20"},
		{nil, false, "00"},
	}
	for _, c := range cases {
		got := AppendHexPrefix(nil, c.nib, c.leaf)
		if hex.EncodeToString(got) != c.want {
			t.Fatalf("HP(%x, leaf=%v) = %x, want %s", c.nib, c.leaf, got, c.want)
		}
		if HexPrefixLen(len(c.nib)) != len(got) {
			t.Fatalf("HexPrefixLen(%d) = %d, want %d", len(c.nib), HexPrefixLen(len(c.nib)), len(got))
		}
		nib, leaf, err := DecodeHexPrefix(got)
		if err != nil || leaf != c.leaf || !bytes.Equal(nib, c.nib) {
			t.Fatalf("DecodeHexPrefix(%x) = %x, %v, %v", got, nib, leaf, err)
		}
	}
}

func TestSBE(t *testing.T) {
	for _, c := range []struct {
		v    uint64
		want string
	}{{0, "00"}, {1, "01"}, {0xff, "ff"}, {0x100, "0100"}, {FirstDynamicVIDU64(), "0111111112"}, {math.MaxUint64, "ffffffffffffffff"}} {
		got := AppendSBE(nil, c.v)
		if hex.EncodeToString(got) != c.want || SBELen(c.v) != len(got) {
			t.Fatalf("SBE(%d) = %x (len %d), want %s", c.v, got, SBELen(c.v), c.want)
		}
		back, err := DecodeSBE(got)
		if err != nil || back != c.v {
			t.Fatalf("DecodeSBE(%x) = %d, %v", got, back, err)
		}
	}
	if _, err := DecodeSBE(make([]byte, 9)); err == nil {
		t.Fatal("DecodeSBE accepted 9 bytes")
	}
}

// FirstDynamicVIDU64 keeps the SBE table readable.
func FirstDynamicVIDU64() uint64 { return uint64(FirstDynamicVID) }

// The worked example from aristo_blobify.nim: depth-1 account leaf, nonce 1,
// balance 1e18, no storage, no code.
func TestAccLeafVector(t *testing.T) {
	full := ethrex.BytesToNibbles(crypto.Keccak256(common.HexToAddress("0x1000000000000000000000000000000000000001").Bytes()))
	pfx := full[1:]
	payload := AppendAccLeafPayload(nil, 1, uint256.NewInt(1e18), 0, 0, EmptyCodeHash)
	if want := mustHex(t, "010de0b6b3a7640000003803"); !bytes.Equal(payload, want) {
		t.Fatalf("payload = %x, want %x", payload, want)
	}
	rec := AppendLeafRecord(nil, payload, pfx)
	if len(rec) != len(payload)+32+1 {
		t.Fatalf("record length %d", len(rec))
	}
	if rec[len(payload)] != 0x30|pfx[0] {
		t.Fatalf("HP flag byte %02x", rec[len(payload)])
	}
	if rec[len(rec)-1] != 0x60 {
		t.Fatalf("trailer %02x, want 60", rec[len(rec)-1])
	}
	v, err := DecodeVertex(rec)
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != AccLeaf || v.Nonce != 1 || !v.Balance.Eq(uint256.NewInt(1e18)) || v.StoID != 0 || v.StoHint != 0 || v.CodeHash != EmptyCodeHash || !bytes.Equal(v.Pfx, pfx) || v.Key != nil {
		t.Fatalf("decoded %+v", v)
	}
	if !bytes.Equal(v.Encode(), rec) {
		t.Fatal("re-encode differs")
	}
}

func TestEmptyPrefixLeaf(t *testing.T) {
	rec := AppendLeafRecord(nil, AppendStoLeafPayload(nil, common.HexToHash("0xaa")), nil)
	if want := mustHex(t, "aa202041"); !bytes.Equal(rec, want) {
		t.Fatalf("record %x, want %x", rec, want)
	}
	v, err := DecodeVertex(rec)
	if err != nil || v.Type != StoLeaf || len(v.Pfx) != 0 || v.StoValue != common.HexToHash("0xaa") {
		t.Fatalf("decoded %+v, %v", v, err)
	}
}

func TestStoLeafPayloadFromRLP(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	values := []common.Hash{common.HexToHash("0x01"), common.HexToHash("0x7f"), common.HexToHash("0x80"), common.HexToHash("0xff"), common.HexToHash("0x0100")}
	for i := 0; i < 500; i++ {
		var v common.Hash
		rng.Read(v[:])
		for z := rng.Intn(32); z > 0; z-- {
			v[z-1] = 0
		}
		if v == (common.Hash{}) {
			v[31] = 1
		}
		values = append(values, v)
	}
	for _, v := range values {
		direct := AppendStoLeafPayload(nil, v)
		fromRLP := AppendStoLeafPayloadFromRLP(nil, ethrex.EncodeStorageValueBytes32(v))
		if !bytes.Equal(direct, fromRLP) {
			t.Fatalf("value %x: direct %x vs fromRLP %x", v, direct, fromRLP)
		}
	}
}

func randomVertex(rng *rand.Rand) Vertex {
	pfx := make([]byte, rng.Intn(64))
	for i := range pfx {
		pfx[i] = byte(rng.Intn(16))
	}
	switch rng.Intn(4) {
	case 0, 1:
		v := Vertex{Type: Branch, StartVid: VertexID(rng.Uint64()>>uint(rng.Intn(64)) + 1), Used: uint16(rng.Uint32())}
		if rng.Intn(2) == 0 {
			v.Key = make([]byte, 32)
			rng.Read(v.Key)
		}
		if len(pfx) > 0 && rng.Intn(2) == 0 {
			v.Type = ExtBranch
			v.Pfx = pfx
		}
		return v
	case 2:
		v := Vertex{Type: AccLeaf, Pfx: pfx, Balance: new(uint256.Int), CodeHash: EmptyCodeHash}
		if rng.Intn(2) == 0 {
			v.Nonce = rng.Uint64() >> uint(rng.Intn(64))
		}
		if rng.Intn(2) == 0 {
			var b [32]byte
			rng.Read(b[:])
			v.Balance.SetBytes(b[rng.Intn(32):])
		}
		if rng.Intn(2) == 0 {
			v.StoID = FirstDynamicVID + VertexID(rng.Intn(1<<20))
			v.StoHint = byte(1 + rng.Intn(int(MaxStoHint)))
		}
		if rng.Intn(2) == 0 {
			rng.Read(v.CodeHash[:])
		}
		return v
	default:
		v := Vertex{Type: StoLeaf, Pfx: pfx}
		rng.Read(v.StoValue[:])
		for z := rng.Intn(32); z > 0; z-- {
			v.StoValue[z-1] = 0
		}
		if v.StoValue == (common.Hash{}) {
			v.StoValue[31] = 1
		}
		return v
	}
}

func TestVertexRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 5000; i++ {
		v := randomVertex(rng)
		rec := v.Encode()
		if len(rec) > MaxVertexBlobSize {
			t.Fatalf("record %d bytes for %+v", len(rec), v)
		}
		back, err := DecodeVertex(rec)
		if err != nil {
			t.Fatalf("decode %x (%+v): %v", rec, v, err)
		}
		if !bytes.Equal(back.Encode(), rec) {
			t.Fatalf("round trip mismatch for %+v: %x vs %x", v, back.Encode(), rec)
		}
		if back.Type != v.Type || !bytes.Equal(back.Pfx, v.Pfx) || back.StartVid != v.StartVid || back.Used != v.Used ||
			!bytes.Equal(back.Key, v.Key) || back.Nonce != v.Nonce || back.StoID != v.StoID || back.StoHint != v.StoHint ||
			back.CodeHash != v.CodeHash || back.StoValue != v.StoValue {
			t.Fatalf("decoded %+v != %+v", back, v)
		}
		if v.Type == AccLeaf && !back.Balance.Eq(v.Balance) {
			t.Fatalf("balance %s != %s", back.Balance, v.Balance)
		}
	}
}

func TestAccLeafBoundary118(t *testing.T) {
	pfx := bytes.Repeat([]byte{0xa}, 64)
	var code common.Hash
	code[0] = 1
	payload := AppendAccLeafPayload(nil, math.MaxUint64, new(uint256.Int).SetAllOne(), VertexID(math.MaxUint64), MaxStoHint, code)
	rec := AppendLeafRecord(nil, payload, pfx)
	if len(rec) != MaxVertexBlobSize {
		t.Fatalf("largest account leaf is %d bytes, want %d", len(rec), MaxVertexBlobSize)
	}
	if _, err := DecodeVertex(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeVertex(append([]byte{0}, rec...)); err == nil {
		t.Fatal("119-byte record accepted")
	}
	hint := AppendLeafRecord(nil, AppendAccLeafPayload(nil, 0, nil, FirstDynamicVID, MaxStoHint+1, EmptyCodeHash), pfx)
	if _, err := DecodeVertex(hint); err == nil {
		t.Fatal("stoHint 10 accepted")
	}
}

func TestSavedState(t *testing.T) {
	got := EncodeSavedState(SavedState{VTop: uint64(FirstDynamicVID)})
	if want := mustHex(t, "000000011111111200000000000000007e"); !bytes.Equal(got, want) {
		t.Fatalf("admin record %x, want %x", got, want)
	}
	s, err := DecodeSavedState(got)
	if err != nil || s.VTop != uint64(FirstDynamicVID) || s.Serial != 0 {
		t.Fatalf("decoded %+v, %v", s, err)
	}
	legacy := append(append([]byte(nil), got[:16]...), SavedStateLegacyMagic)
	if _, err := DecodeSavedState(legacy); err != nil {
		t.Fatalf("legacy trailer rejected: %v", err)
	}
	if _, err := DecodeSavedState(got[:16]); err == nil {
		t.Fatal("short record accepted")
	}
}

func TestVidAllocator(t *testing.T) {
	a := NewVidAllocator()
	if a.VTop() != 0 {
		t.Fatalf("fresh VTop %d", a.VTop())
	}
	if got := a.Alloc(1); got != FirstDynamicVID {
		t.Fatalf("first Alloc = %d", got)
	}
	if a.VTop() != uint64(FirstDynamicVID) {
		t.Fatalf("VTop after one = %d", a.VTop())
	}
	if got := a.Alloc(16); got != FirstDynamicVID+1 {
		t.Fatalf("block Alloc = %d", got)
	}
	if a.VTop() != uint64(FirstDynamicVID)+16 {
		t.Fatalf("VTop after block = %d", a.VTop())
	}
}

func TestKvtKeys(t *testing.T) {
	h := common.HexToHash("0x11")
	if !bytes.Equal(KeyCanonicalHead, []byte{4, 0}) {
		t.Fatalf("canonical head key %x", KeyCanonicalHead)
	}
	if got := KeyBlockNumToHash(0x0102); !bytes.Equal(got, mustHex(t, "010201000000000000")) {
		t.Fatalf("block-number key %x", got)
	}
	if got := KeyFcu(FcuHead); !bytes.Equal(got, mustHex(t, "080000000000000000")) {
		t.Fatalf("fcu key %x", got)
	}
	for kind, key := range map[byte][]byte{0: KeyHeader(h), 2: KeyScore(h), 6: KeyCode(h)} {
		if len(key) != 33 || key[0] != kind || !bytes.Equal(key[1:], h[:]) {
			t.Fatalf("kind %d key %x", kind, key)
		}
	}
	if got := RLPHash(h); len(got) != 33 || got[0] != 0xa0 || !bytes.Equal(got[1:], h[:]) {
		t.Fatalf("RLPHash %x", got)
	}
	if got := FcuValue(0, h); !bytes.Equal(got[:8], make([]byte, 8)) || !bytes.Equal(got[8:], h[:]) {
		t.Fatalf("FcuValue %x", got)
	}
}
