package nimbus

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// verify_fixture_test.go checks the codec against output of nimbus itself:
// testdata/genesis_dump.json was produced by nimbus's genesis path (see
// testdata/gen/README.md). Every row must decode, the Merkle keys must
// re-derive to the header's state root, and the layout must satisfy the
// static/dynamic vertex-ID rules — the same checks client/nimbus's golden
// test runs over the writer's output, here on the real thing and without cgo.

type dumpFile struct {
	CFs map[string][][2]string `json:"cfs"`
}

func loadFixture(t *testing.T) dumpFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/genesis_dump.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var d dumpFile
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return d
}

func fixtureBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("fixture hex %q: %v", s, err)
	}
	return b
}

func TestFixtureVerifies(t *testing.T) {
	d := loadFixture(t)

	// The genesis header row carries the expected state root.
	var header *types.Header
	var codeRows int
	for _, kv := range d.CFs[CFKvtGen] {
		key, val := fixtureBytes(t, kv[0]), fixtureBytes(t, kv[1])
		switch {
		case len(key) == 33 && key[0] == 0x00:
			var h types.Header
			if err := gethrlp.DecodeBytes(val, &h); err != nil {
				t.Fatalf("decode header: %v", err)
			}
			header = &h
		case len(key) == 33 && key[0] == 0x06:
			codeRows++
		}
	}
	if header == nil {
		t.Fatal("no header row in KvtGen")
	}
	if codeRows != 2 {
		t.Fatalf("expected 2 code rows, found %d", codeRows)
	}

	rows := VertexRows{}
	var admin SavedState
	for _, kv := range d.CFs[CFAriVtx] {
		if err := CollectRow(rows, &admin, fixtureBytes(t, kv[0]), fixtureBytes(t, kv[1])); err != nil {
			t.Fatalf("row %s: %v", kv[0], err)
		}
	}
	rep, err := VerifyState(rows, admin, header.Root)
	if err != nil {
		t.Fatalf("VerifyState over nimbus's own output: %v", err)
	}
	if rep.Accounts != 3 || rep.StorageTries != 1 || rep.StorageLeaves != 3 {
		t.Fatalf("unexpected fixture shape: %+v", *rep)
	}
	if admin.VTop != uint64(FirstDynamicVID) || admin.Serial != 0 {
		t.Fatalf("admin record %+v", admin)
	}
	// Round-trip every record through the codec byte-for-byte.
	for rvid, rec := range rows {
		v, err := DecodeVertex(rec)
		if err != nil {
			t.Fatalf("decode %+v: %v", rvid, err)
		}
		if got := v.Encode(); string(got) != string(rec) {
			t.Fatalf("re-encode of %+v differs:\n got  %x\n want %x", rvid, got, rec)
		}
	}
}

// The Kvt rows nimbus writes at genesis are exactly the ones kvt.go models.
func TestFixtureKvtKeys(t *testing.T) {
	d := loadFixture(t)
	var headerHash [32]byte
	seen := map[string]bool{}
	for _, kv := range d.CFs[CFKvtGen] {
		key := fixtureBytes(t, kv[0])
		if len(key) == 33 && key[0] == 0x00 {
			copy(headerHash[:], key[1:])
		}
		seen[kv[0]] = true
	}
	want := map[string][]byte{
		"canonical head": KeyCanonicalHead,
		"block 0 → hash": KeyBlockNumToHash(0),
		"fcu head":       KeyFcu(FcuHead),
		"score":          KeyScore(headerHash),
		"header":         KeyHeader(headerHash),
	}
	for name, key := range want {
		if !seen["0x"+hex.EncodeToString(key)] {
			t.Fatalf("fixture lacks the %s row (%x)", name, key)
		}
	}
	for _, kv := range d.CFs[CFKvtGen] {
		key, val := fixtureBytes(t, kv[0]), fixtureBytes(t, kv[1])
		switch {
		case string(key) == string(KeyCanonicalHead), string(key) == string(KeyBlockNumToHash(0)):
			if string(val) != string(RLPHash(headerHash)) {
				t.Fatalf("row %x value %x != RLPHash", key, val)
			}
		case string(key) == string(KeyScore(headerHash)):
			if string(val) != string(RLPZeroScore) {
				t.Fatalf("score row %x", val)
			}
		case string(key) == string(KeyFcu(FcuHead)):
			if string(val) != string(FcuValue(0, headerHash)) {
				t.Fatalf("fcu row %x", val)
			}
		}
	}
}
