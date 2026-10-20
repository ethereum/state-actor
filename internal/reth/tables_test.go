package reth

import (
	"sort"
	"testing"
)

// expectedTableManifest mirrors the schema explorer report Section A,
// double-checked against crates/storage/db-api/src/tables/mod.rs:308-540.
// If reth adds/renames/removes a table, this manifest update + table
// registry update + golden hex regen are all required.
var expectedTableManifest = map[string]bool{
	"CanonicalHeaders":            false,
	"HeaderTerminalDifficulties":  false,
	"HeaderNumbers":               false,
	"Headers":                     false,
	"BlockBodyIndices":            false,
	"BlockOmmers":                 false,
	"BlockWithdrawals":            false,
	"BlockAccessLists":            false, // reth v2.7.0
	"BlockAccessListBlockNumbers": false, // reth v2.7.0
	"Transactions":                false,
	"TransactionHashNumbers":      false,
	"TransactionBlocks":           false,
	"Receipts":                    false,
	"Bytecodes":                   false,
	"PlainAccountState":           false,
	"PlainStorageState":           true, // DupSort
	"AccountsHistory":             false,
	"StoragesHistory":             false,
	"AccountChangeSets":           true, // DupSort
	"StorageChangeSets":           true, // DupSort
	"HashedAccounts":              false,
	"HashedStorages":              true, // DupSort
	"AccountsTrie":                false,
	"StoragesTrie":                true, // DupSort
	"TransactionSenders":          false,
	"StageCheckpoints":            false,
	"StageCheckpointProgresses":   false,
	"PruneCheckpoints":            false,
	"VersionHistory":              false,
	"ChainState":                  false,
	"Metadata":                    false,
}

func TestTableManifestMatchesRegistry(t *testing.T) {
	registryNames := make(map[string]bool)
	for _, ts := range Tables {
		if _, dup := registryNames[ts.Name]; dup {
			t.Errorf("duplicate table in registry: %s", ts.Name)
		}
		registryNames[ts.Name] = ts.DupSort
	}

	// Check every expected table is in the registry with the right DupSort flag.
	for name, dup := range expectedTableManifest {
		gotDup, ok := registryNames[name]
		if !ok {
			t.Errorf("missing table in registry: %s", name)
			continue
		}
		if gotDup != dup {
			t.Errorf("table %s: registry DupSort=%v, manifest DupSort=%v", name, gotDup, dup)
		}
	}

	// Check no unexpected tables in the registry.
	for name := range registryNames {
		if _, ok := expectedTableManifest[name]; !ok {
			t.Errorf("unexpected table in registry: %s", name)
		}
	}

	// Friendly: log the table count.
	count := len(Tables)
	expected := len(expectedTableManifest)
	if count != expected {
		t.Errorf("Tables count = %d, manifest count = %d", count, expected)
	}

	// Friendly: print sorted name list for visual inspection on -v.
	names := make([]string, 0, len(registryNames))
	for n := range registryNames {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("registered tables (%d):", len(names))
	for _, n := range names {
		t.Logf("  %s (DupSort=%v)", n, registryNames[n])
	}
}
