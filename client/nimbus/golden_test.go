//go:build cgo_nimbus

package nimbus_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/linxGnu/grocksdb"

	clientnimbus "github.com/ethereum/state-actor/client/nimbus"
	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/e2e_testing"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

// cfRow is one raw (key, value) pair read back from the written DB.
type cfRow struct{ key, value []byte }

// readAllCFs reopens a finished nimbus DB read-only and returns every row of
// every column family, in key order (RocksDB's bytewise iteration order).
func readAllCFs(t *testing.T, ecdbDir string) map[string][]cfRow {
	t.Helper()
	names := nimbusinternal.ColumnFamilies
	opts := grocksdb.NewDefaultOptions()
	defer opts.Destroy()
	cfOpts := make([]*grocksdb.Options, len(names))
	for i := range cfOpts {
		cfOpts[i] = grocksdb.NewDefaultOptions()
		defer cfOpts[i].Destroy()
	}
	db, cfs, err := grocksdb.OpenDbForReadOnlyColumnFamilies(opts, ecdbDir, names, cfOpts, false)
	if err != nil {
		t.Fatalf("reopen %s read-only: %v", ecdbDir, err)
	}
	defer db.Close()
	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	out := make(map[string][]cfRow, len(names))
	for i, name := range names {
		it := db.NewIteratorCF(ro, cfs[i])
		for it.SeekToFirst(); it.Valid(); it.Next() {
			k, v := it.Key(), it.Value()
			out[name] = append(out[name], cfRow{
				key:   append([]byte(nil), k.Data()...),
				value: append([]byte(nil), v.Data()...),
			})
			k.Free()
			v.Free()
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iterate %s: %v", name, err)
		}
		it.Close()
		cfs[i].Destroy()
	}
	return out
}

// collectVertices files the AriVtx rows into internal/nimbus's verifier input.
func collectVertices(t *testing.T, rows []cfRow) (nimbusinternal.VertexRows, nimbusinternal.SavedState) {
	t.Helper()
	vrows := make(nimbusinternal.VertexRows, len(rows))
	var admin nimbusinternal.SavedState
	sawAdmin := false
	for _, r := range rows {
		if len(r.key) == 0 {
			sawAdmin = true
		}
		if err := nimbusinternal.CollectRow(vrows, &admin, r.key, r.value); err != nil {
			t.Fatalf("AriVtx row %x: %v", r.key, err)
		}
	}
	if !sawAdmin {
		t.Fatal("AriVtx has no admin record (empty key)")
	}
	return vrows, admin
}

// TestNimbusGoldenStateRoot pins the writer to entitygen.CanonicalOsakaMPTRoot
// for the canonical auto-fill config, then reopens the DB and runs
// internal/nimbus.VerifyState over every vertex row: re-derived Merkle keys,
// static/dynamic vertex placement, stoID/stoHint, vTop, and every leaf
// resolvable the way nimbus's retrieveStatic reads it. Together with the
// read-only reopen after Close() (zstd bottommost SSTs), this is the
// strongest boot-free check of the produced database.
func TestNimbusGoldenStateRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nimbus-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := e2e_testing.GoldenStateRootCfg(dbPath)
	var stats *generator.Stats
	e2e_testing.AssertGoldenStateRoot(t, "nimbus", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			s, err := clientnimbus.Run(ctx, cfg, clientnimbus.Options{})
			stats = s
			return s, err
		})

	all := readAllCFs(t, clientnimbus.StoreDir(dbPath))
	vrows, admin := collectVertices(t, all[nimbusinternal.CFAriVtx])
	rep, err := nimbusinternal.VerifyState(vrows, admin, stats.StateRoot)
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	t.Logf("verified %d vertex rows: %+v (vTop %d)", len(vrows), *rep, admin.VTop)
	if rep.DynamicVertices == 0 || rep.StorageTries == 0 {
		t.Fatalf("golden config produced no storage tries: %+v", *rep)
	}
	if rep.Accounts != stats.AccountsCreated+stats.ContractsCreated {
		t.Fatalf("verifier saw %d accounts, writer reported %d EOAs + %d contracts", rep.Accounts, stats.AccountsCreated, stats.ContractsCreated)
	}
	codeRows := 0
	for _, r := range all[nimbusinternal.CFKvtGen] {
		if len(r.key) == 33 && r.key[0] == 0x06 {
			codeRows++
			if len(r.value) == 0 {
				t.Fatalf("empty code row %x", r.key)
			}
		}
	}
	if codeRows == 0 {
		t.Fatal("no code rows in KvtGen")
	}
	if len(all[nimbusinternal.CFKvtSync]) != 0 || len(all[nimbusinternal.CFDefault]) != 0 {
		t.Fatalf("KvtSync/default must be empty: %d / %d rows", len(all[nimbusinternal.CFKvtSync]), len(all[nimbusinternal.CFDefault]))
	}
	if _, err := os.Stat(filepath.Join(dbPath, clientnimbus.GenesisFileName)); err != nil {
		t.Fatalf("genesis sidecar: %v", err)
	}
}
