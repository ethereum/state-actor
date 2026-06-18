//go:build cgo_ethrex

package ethrex_test

// genesis_dump_cgo_test.go exercises the full ethrex DB writer against
// the spike fixture (internal/ethrex/testdata/gen/genesis.json) and
// diffs every CF against genesis_dump.json.
//
// This test only compiles with -tags cgo_ethrex (requires librocksdb) and
// cannot run locally without it. It is included in the Docker build.
// The header golden test (TestGenesisHeaderGolden in genesis_header_test.go)
// runs locally without cgo and validates the header path independently.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/client/ethrex"
	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
)

func TestGenesisDumpGolden(t *testing.T) {
	// Build genesis matching testdata/gen/genesis.json.
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 0x17d7840, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	// Build genesis accounts matching genesis.json alloc.
	addr1 := common.HexToAddress("0x1000000000000000000000000000000000000001")
	addr2 := common.HexToAddress("0x2000000000000000000000000000000000000002")
	addr3 := common.HexToAddress("0x3000000000000000000000000000000000000003")

	bal1, _ := new(big.Int).SetString("3635c9adc5dea00000", 16)
	bal1u, _ := uint256.FromBig(bal1)
	bal2u := uint256.NewInt(0x64)
	bal3u := uint256.NewInt(0)

	cfg := generator.Config{
		DBPath:  t.TempDir(),
		Seed:    1,
		Genesis: g,
		GenesisAccounts: map[common.Address]*types.StateAccount{
			addr1: {Nonce: 7, Balance: bal1u},
			addr2: {Nonce: 1, Balance: bal2u},
			addr3: {Nonce: 0, Balance: bal3u},
		},
		GenesisCode: map[common.Address][]byte{
			addr2: goldenHexBytes("600160015500"),
			addr3: goldenHexBytes("60015b00"),
		},
		GenesisStorage: map[common.Address]map[common.Hash]common.Hash{
			addr2: {
				slotKey(0): common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000000aa"),
				slotKey(1): common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000000bb"),
				slotKey(2): common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000cccccc"),
			},
		},
	}

	stats, err := ethrex.Run(context.Background(), cfg, ethrex.Options{})
	if err != nil {
		t.Fatalf("ethrex.Run: %v", err)
	}

	// Verify state root.
	wantRoot := common.HexToHash("0x7acb3fa2fa378c411135fef796a61c4c681c14e9c844e4af1d779a6f6a7006fb")
	if stats.StateRoot != wantRoot {
		t.Errorf("state root: got %s, want %s", stats.StateRoot.Hex(), wantRoot.Hex())
	}

	// Load dump.
	dump := loadGoldenDump(t)

	// Diff each written CF against the dump. The writer nests the store under
	// chain-<chainid> (see ethrex.StoreDir), so read from there, not cfg.DBPath.
	dbPath := ethrex.StoreDir(cfg.DBPath, g)
	for _, cfName := range []string{
		"account_codes",
		"account_code_metadata",
		"headers",
		"bodies",
		"block_numbers",
		"canonical_block_hashes",
	} {
		wantRows := dump[cfName]
		diffCF(t, dbPath, cfName, wantRows)
	}

	// Snap-sync layout: the trie-node CFs hold ONLY structural + leaf-NODE-RLP
	// rows; the leaf full-path rows (keys ending in the leaf-flag nibble 0x10)
	// live solely in the flat-KV CFs, never duplicated in the trie-node CFs. So
	// the expected trie-node set is the genesis dump MINUS those leaf rows, and
	// the flat-KV set is exactly those leaf rows. (The genesis dump itself carries
	// the leaf rows in the trie-node CFs because it captures a genesis-booted
	// node; a snap-synced node — which we model — does not.)
	diffCF(t, dbPath, "account_trie_nodes", nonLeafFullPathRows(dump["account_trie_nodes"]))
	diffCF(t, dbPath, "storage_trie_nodes", nonLeafFullPathRows(dump["storage_trie_nodes"]))
	diffCF(t, dbPath, "account_flatkeyvalue", leafFullPathRows(dump["account_trie_nodes"]))
	diffCF(t, dbPath, "storage_flatkeyvalue", leafFullPathRows(dump["storage_trie_nodes"]))

	// misc_values must carry the FKV "fully generated" sentinel so ethrex skips
	// regeneration on boot.
	diffCF(t, dbPath, "misc_values", []dumpRow{
		{key: []byte(ethrexinternal.MiscValuesLastWrittenKey), val: ethrexinternal.FKVLastWrittenComplete},
	})

	// chain_data needs special handling: the ChainConfig value (key 0x80) is
	// ethrex's own serialization which we do NOT reproduce byte-for-byte (and
	// don't need to — ethrex rewrites it from --network on every boot, before
	// the boot-gate check). We assert the LE block-number keys byte-exact and
	// that the ChainConfig key holds parseable JSON.
	diffChainData(t, dbPath, dump["chain_data"])
}

// leafFullPathRows returns the subset of trie-node rows that are leaf full-path
// rows: those whose key ends in the leaf-flag nibble (0x10). These rows are
// byte-identical to the flat-KV entries (same key, same value), so they double
// as the golden expectation for the flat-KV CFs.
func leafFullPathRows(rows []dumpRow) []dumpRow {
	out := make([]dumpRow, 0, len(rows))
	for _, r := range rows {
		if len(r.key) > 0 && r.key[len(r.key)-1] == ethrexinternal.LeafFlag {
			out = append(out, r)
		}
	}
	return out
}

// nonLeafFullPathRows is the complement of leafFullPathRows: the structural and
// leaf-NODE-RLP rows that remain in the trie-node CFs under the snap-sync layout.
func nonLeafFullPathRows(rows []dumpRow) []dumpRow {
	out := make([]dumpRow, 0, len(rows))
	for _, r := range rows {
		if !(len(r.key) > 0 && r.key[len(r.key)-1] == ethrexinternal.LeafFlag) {
			out = append(out, r)
		}
	}
	return out
}

// diffChainData verifies chain_data: keys 0x01 (EarliestBlockNumber) and 0x04
// (LatestBlockNumber) must byte-match the dump (u64-LE 0); key 0x80
// (ChainConfig) must be present and valid JSON (not byte-exact, since ethrex
// rewrites it from --network on boot).
func diffChainData(t *testing.T, dbPath string, wantRows []dumpRow) {
	t.Helper()
	got := readCFRows(t, dbPath, "chain_data")
	want := make(map[string]string, len(wantRows))
	for _, row := range wantRows {
		want[hex.EncodeToString(row.key)] = hex.EncodeToString(row.val)
	}
	for _, key := range []string{"01", "04"} {
		if got[key] != want[key] {
			t.Errorf("chain_data key %s:\n got  %s\n want %s", key, got[key], want[key])
		}
	}
	cfgHex, ok := got["80"]
	if !ok {
		t.Errorf("chain_data: missing ChainConfig key 0x80")
		return
	}
	cfgBytes, err := hex.DecodeString(cfgHex)
	if err != nil || !json.Valid(cfgBytes) {
		t.Errorf("chain_data[0x80]: not valid JSON (err=%v)", err)
	}
}

// slotKey returns the RAW storage slot position (bytes32(n)). GenesisStorage
// keys are raw slot positions; the writer keccak-hashes them internally to form
// the trie path (matching geth/besu). Do NOT pre-hash here.
func slotKey(n uint64) common.Hash {
	var b [32]byte
	b[31] = byte(n)
	return common.Hash(b)
}

func goldenHexBytes(s string) []byte {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		panic(err)
	}
	return b
}

type dumpRow struct {
	key []byte
	val []byte
}

func loadGoldenDump(t *testing.T) map[string][]dumpRow {
	t.Helper()
	data, err := os.ReadFile("../../internal/ethrex/testdata/genesis_dump.json")
	if err != nil {
		t.Fatalf("read genesis_dump.json: %v", err)
	}
	var raw struct {
		CFs map[string][][2]string `json:"cfs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal genesis_dump.json: %v", err)
	}
	result := make(map[string][]dumpRow, len(raw.CFs))
	for cf, rows := range raw.CFs {
		dr := make([]dumpRow, len(rows))
		for i, pair := range rows {
			dr[i] = dumpRow{
				key: mustHexGolden(t, pair[0]),
				val: mustHexGolden(t, pair[1]),
			}
		}
		result[cf] = dr
	}
	return result
}

func mustHexGolden(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode %q: %v", s, err)
	}
	return b
}

// readCFRows opens the DB read-only and returns the named CF's rows as a
// hex(key)->hex(value) map.
func readCFRows(t *testing.T, dbPath, cfName string) map[string]string {
	t.Helper()

	dbOpts := grocksdb.NewDefaultOptions()
	defer dbOpts.Destroy()

	cfNames := []string{"default"}
	for _, name := range ethrexinternal.Tables {
		cfNames = append(cfNames, name)
	}
	cfOpts := make([]*grocksdb.Options, len(cfNames))
	for i := range cfNames {
		cfOpts[i] = grocksdb.NewDefaultOptions()
		defer cfOpts[i].Destroy()
	}

	db, cfHandles, err := grocksdb.OpenDbForReadOnlyColumnFamilies(dbOpts, dbPath, cfNames, cfOpts, false)
	if err != nil {
		t.Fatalf("open DB read-only: %v", err)
	}
	defer db.Close()
	for _, h := range cfHandles {
		defer h.Destroy()
	}

	var cfHandle *grocksdb.ColumnFamilyHandle
	for i, name := range cfNames {
		if name == cfName {
			cfHandle = cfHandles[i]
			break
		}
	}
	if cfHandle == nil {
		t.Fatalf("CF %q not found in DB", cfName)
	}

	roOpts := grocksdb.NewDefaultReadOptions()
	defer roOpts.Destroy()
	iter := db.NewIteratorCF(roOpts, cfHandle)
	defer iter.Close()

	gotMap := make(map[string]string)
	for iter.SeekToFirst(); iter.Valid(); iter.Next() {
		k := iter.Key()
		v := iter.Value()
		gotMap[hex.EncodeToString(k.Data())] = hex.EncodeToString(v.Data())
		k.Free()
		v.Free()
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("CF %s iterator error: %v", cfName, err)
	}
	return gotMap
}

func diffCF(t *testing.T, dbPath, cfName string, wantRows []dumpRow) {
	t.Helper()

	gotMap := readCFRows(t, dbPath, cfName)

	wantMap := make(map[string]string, len(wantRows))
	for _, row := range wantRows {
		wantMap[hex.EncodeToString(row.key)] = hex.EncodeToString(row.val)
	}

	for k, wv := range wantMap {
		gv, ok := gotMap[k]
		if !ok {
			t.Errorf("CF %s: missing key %s (want val %s)", cfName, k, wv)
			continue
		}
		if gv != wv {
			t.Errorf("CF %s key %s:\n got  %s\n want %s", cfName, k, gv, wv)
		}
	}
	for k := range gotMap {
		if _, ok := wantMap[k]; !ok {
			t.Errorf("CF %s: unexpected key %s", cfName, k)
		}
	}
}
