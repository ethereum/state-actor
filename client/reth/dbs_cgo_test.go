//go:build cgo_reth

package reth

import (
	"os"
	"path/filepath"
	"testing"

	iReth "github.com/ethereum/state-actor/internal/reth"
)

func TestOpenEnvsFreshDir(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()
	if _, err := os.Stat(filepath.Join(tmp, "db", "mdbx.dat")); err != nil {
		t.Errorf("mdbx.dat not created: %v", err)
	}
	for _, ts := range iReth.Tables {
		if _, ok := envs.MdbxDBIs[ts.Name]; !ok {
			t.Errorf("missing DBI for table %q", ts.Name)
		}
	}
	for _, name := range []string{"default", "AccountsHistory", "StoragesHistory", "TransactionHashNumbers"} {
		if _, ok := envs.RocksCFs[name]; !ok {
			t.Errorf("missing RocksDB CF %q", name)
		}
	}
}

func TestOpenEnvsRefusesExistingDir(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, tmp string)
	}{
		{
			name: "preexisting mdbx.dat",
			setup: func(t *testing.T, tmp string) {
				dir := filepath.Join(tmp, "db")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "mdbx.dat"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "preexisting rocksdb/CURRENT",
			setup: func(t *testing.T, tmp string) {
				dir := filepath.Join(tmp, "rocksdb")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "preexisting static_files",
			setup: func(t *testing.T, tmp string) {
				if err := os.MkdirAll(filepath.Join(tmp, "static_files"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			tc.setup(t, tmp)
			_, err := OpenEnvs(tmp, true)
			if err == nil {
				t.Errorf("OpenEnvs(freshDir=true) should error on %s", tc.name)
			}
		})
	}
}

func TestOpenEnvsAllDBIsCount(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()
	// Catch silent duplicate names in iReth.Tables that would map-collide.
	if got, want := len(envs.MdbxDBIs), len(iReth.Tables); got != want {
		t.Errorf("MdbxDBIs count = %d, want %d (silent duplicate in Tables?)", got, want)
	}
}

func TestOpenEnvsCloseIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	if err := envs.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := envs.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
