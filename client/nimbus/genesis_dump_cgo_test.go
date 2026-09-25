//go:build cgo_nimbus

package nimbus_test

// genesis_dump_cgo_test.go runs the writer against the 3-account fixture
// (internal/nimbus/testdata/gen/genesis.json) and compares every AriVtx and
// KvtGen row byte-for-byte with internal/nimbus/testdata/genesis_dump.json,
// which nimbus's own genesis path produced (see the fixture README).
//
// The one normalised byte is the AccLeaf stoHint: nimbus derives it from its
// slot insertion order, state-actor from the shallowest leaf; both are valid
// (any 1..9 is), so the comparison rewrites it to 1 on both sides.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"

	clientnimbus "github.com/ethereum/state-actor/client/nimbus"
	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

const fixtureDumpPath = "../../internal/nimbus/testdata/genesis_dump.json"

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func slotKey(i uint64) common.Hash { return common.BigToHash(new(big.Int).SetUint64(i)) }

// loadDump parses a genesis_dump.json: {"cfs": {"<cf>": [[hexKey, hexVal], ...]}}.
func loadDump(t *testing.T, path string) map[string][]cfRow {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		CFs map[string][][2]string `json:"cfs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := make(map[string][]cfRow, len(doc.CFs))
	for cf, pairs := range doc.CFs {
		for _, p := range pairs {
			out[cf] = append(out[cf], cfRow{key: mustHexBytes(t, p[0]), value: mustHexBytes(t, p[1])})
		}
	}
	return out
}

// normaliseStoHint rewrites the stoHint of every storage-bearing AccLeaf to 1.
func normaliseStoHint(t *testing.T, rows []cfRow) []cfRow {
	t.Helper()
	out := make([]cfRow, len(rows))
	for i, r := range rows {
		out[i] = r
		if len(r.key) == 0 {
			continue
		}
		v, err := nimbusinternal.DecodeVertex(r.value)
		if err != nil {
			t.Fatalf("decode AriVtx row %x: %v", r.key, err)
		}
		if v.Type == nimbusinternal.AccLeaf && v.StoID != 0 {
			v.StoHint = 1
			out[i].value = v.Encode()
		}
	}
	return out
}

func describeRow(cf string, r cfRow) string {
	if cf == nimbusinternal.CFAriVtx && len(r.key) > 0 {
		if v, err := nimbusinternal.DecodeVertex(r.value); err == nil {
			return fmt.Sprintf("%x → %x (%s pfx=%x startVid=%d used=%04x stoID=%d stoHint=%d)", r.key, r.value, v.Type, v.Pfx, v.StartVid, v.Used, v.StoID, v.StoHint)
		}
	}
	return fmt.Sprintf("%x → %x", r.key, r.value)
}

func diffRows(t *testing.T, cf string, got, want []cfRow) {
	t.Helper()
	wantByKey := make(map[string]cfRow, len(want))
	for _, r := range want {
		wantByKey[string(r.key)] = r
	}
	gotByKey := make(map[string]cfRow, len(got))
	for _, r := range got {
		gotByKey[string(r.key)] = r
	}
	var problems []string
	for _, r := range want {
		g, ok := gotByKey[string(r.key)]
		switch {
		case !ok:
			problems = append(problems, "missing:    "+describeRow(cf, r))
		case !bytes.Equal(g.value, r.value):
			problems = append(problems, "mismatch:   want "+describeRow(cf, r)+"\n            got  "+describeRow(cf, g))
		}
	}
	for _, r := range got {
		if _, ok := wantByKey[string(r.key)]; !ok {
			problems = append(problems, "unexpected: "+describeRow(cf, r))
		}
	}
	if len(problems) > 0 {
		t.Errorf("%s: %d rows differ from the fixture (%d want, %d got):\n%s", cf, len(problems), len(want), len(got), strings.Join(problems, "\n"))
	}
}

func TestGenesisDumpGolden(t *testing.T) {
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 0x17d7840, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	addr1 := common.HexToAddress("0x1000000000000000000000000000000000000001")
	addr2 := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addr3 := common.HexToAddress("0x3000000000000000000000000000000000000003")
	bal1, _ := new(big.Int).SetString("3635c9adc5dea00000", 16)
	bal1u, _ := uint256.FromBig(bal1)

	dbPath := t.TempDir()
	cfg := generator.Config{
		DBPath:  dbPath,
		Seed:    1,
		Genesis: g,
		GenesisAccounts: map[common.Address]*types.StateAccount{
			addr1: {Nonce: 7, Balance: bal1u},
			addr2: {Nonce: 1, Balance: uint256.NewInt(0x64)},
			addr3: {Nonce: 0, Balance: uint256.NewInt(0)},
		},
		GenesisCode: map[common.Address][]byte{
			addr2: mustHexBytes(t, "600160015500"),
			addr3: mustHexBytes(t, "60015b00"),
		},
		GenesisStorage: map[common.Address]map[common.Hash]common.Hash{
			addr2: {
				slotKey(0): common.HexToHash("0xaa"),
				slotKey(1): common.HexToHash("0xbb"),
				slotKey(2): common.HexToHash("0xcccccc"),
			},
		},
	}
	stats, err := clientnimbus.Run(context.Background(), cfg, clientnimbus.Options{})
	if err != nil {
		t.Fatalf("nimbus.Run: %v", err)
	}

	want := loadDump(t, fixtureDumpPath)
	got := readAllCFs(t, clientnimbus.StoreDir(dbPath))

	// The fixture's genesis header pins the expected state root and hash.
	var wantHeader *types.Header
	for _, r := range want[nimbusinternal.CFKvtGen] {
		if len(r.key) == 33 && r.key[0] == 0x00 {
			var h types.Header
			if err := gethrlp.DecodeBytes(r.value, &h); err != nil {
				t.Fatalf("decode fixture header: %v", err)
			}
			wantHeader = &h
		}
	}
	if wantHeader == nil {
		t.Fatal("fixture has no header row")
	}
	if stats.StateRoot != wantHeader.Root {
		t.Fatalf("state root %s, fixture header has %s", stats.StateRoot.Hex(), wantHeader.Root.Hex())
	}

	diffRows(t, nimbusinternal.CFAriVtx,
		normaliseStoHint(t, got[nimbusinternal.CFAriVtx]),
		normaliseStoHint(t, want[nimbusinternal.CFAriVtx]))
	diffRows(t, nimbusinternal.CFKvtGen, got[nimbusinternal.CFKvtGen], want[nimbusinternal.CFKvtGen])
	if len(got[nimbusinternal.CFKvtSync]) != 0 || len(got[nimbusinternal.CFDefault]) != 0 {
		t.Errorf("KvtSync/default must be empty: %d / %d rows", len(got[nimbusinternal.CFKvtSync]), len(got[nimbusinternal.CFDefault]))
	}

	// Structural check on what we wrote, independent of the fixture.
	vrows, admin := collectVertices(t, got[nimbusinternal.CFAriVtx])
	rep, err := nimbusinternal.VerifyState(vrows, admin, stats.StateRoot)
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if rep.Accounts != 3 || rep.StorageTries != 1 || rep.StorageLeaves != 3 || admin.VTop != uint64(nimbusinternal.FirstDynamicVID) {
		t.Fatalf("unexpected shape: %+v vTop=%d", *rep, admin.VTop)
	}
	if _, err := os.Stat(filepath.Join(dbPath, clientnimbus.GenesisFileName)); err != nil {
		t.Fatalf("genesis sidecar: %v", err)
	}
}
