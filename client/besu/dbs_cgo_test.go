//go:build cgo_besu

package besu

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/state-actor/internal/besu/keys"
)

// TestCFIndicesMatchNames pins each cfIdx* constant to its CF name in
// keys.BonsaiCFNames(). The constants index into besuDB.cfs, whose order
// is exactly the BonsaiCFNames open order — a drift between the two would
// silently write genesis data into the wrong column family.
func TestCFIndicesMatchNames(t *testing.T) {
	names := keys.BonsaiCFNames()
	cases := []struct {
		idx  int
		want string
	}{
		{cfIdxDefault, string(keys.CFDefault)},
		{cfIdxBlockchain, string(keys.CFBlockchain)},
		{cfIdxPrivateTransactions, string(keys.CFPrivateTransactions)},
		{cfIdxPrivateState, string(keys.CFPrivateState)},
		{cfIdxAccountInfoState, string(keys.CFAccountInfoState)},
		{cfIdxCodeStorage, string(keys.CFCodeStorage)},
		{cfIdxAccountStorageStorage, string(keys.CFAccountStorageStorage)},
		{cfIdxTrieBranchStorage, string(keys.CFTrieBranchStorage)},
		{cfIdxTrieLogStorage, string(keys.CFTrieLogStorage)},
		{cfIdxVariables, string(keys.CFVariables)},
		{cfIdxGoQuorumPrivateStorage, string(keys.CFGoQuorumPrivateStorage)},
		{cfIdxBackwardSyncHeaders, string(keys.CFBackwardSyncHeaders)},
		{cfIdxBackwardSyncBlocks, string(keys.CFBackwardSyncBlocks)},
		{cfIdxBackwardSyncChain, string(keys.CFBackwardSyncChain)},
		{cfIdxSnapsyncMissingAccountRange, string(keys.CFSnapsyncMissingAccountRange)},
		{cfIdxSnapsyncAccountToFix, string(keys.CFSnapsyncAccountToFix)},
		{cfIdxChainPrunerState, string(keys.CFChainPrunerState)},
	}
	if len(cases) != len(names) {
		t.Fatalf("cfIdx constants cover %d CFs, BonsaiCFNames has %d", len(cases), len(names))
	}
	for _, c := range cases {
		if names[c.idx] != c.want {
			t.Errorf("BonsaiCFNames[%d] = %x, want %x", c.idx, names[c.idx], c.want)
		}
	}
}

// TestEveryColumnFamilyHasBloomFilter guards against reusing a single
// NativeFilterPolicy across CF table options. SetFilterPolicy moves the native
// policy, so reuse silently configures only the first CF and leaves the rest
// with filter_policy=nullptr.
//
// Inspect OPTIONS rather than LOG: RocksDB deliberately stops dumping CF
// options to LOG once the database reaches 10 CFs, while OPTIONS persists the
// complete configuration for every CF.
//
// Match filter_policy=bloomfilter (not bloomfilter:10:false): grocksdb sets
// filters through RocksDB's C API wrapper, whose GetId() returns only Name()
// ("bloomfilter") and drops the bits-per-key suffix a native BloomFilterPolicy
// would serialize.
func TestEveryColumnFamilyHasBloomFilter(t *testing.T) {
	datadir := t.TempDir()
	db, err := openBesuDB(datadir)
	if err != nil {
		t.Fatalf("openBesuDB: %v", err)
	}
	db.Close()

	optionFiles, err := filepath.Glob(filepath.Join(datadir, "database", "OPTIONS-*"))
	if err != nil {
		t.Fatalf("glob RocksDB OPTIONS files: %v", err)
	}
	if len(optionFiles) == 0 {
		t.Fatal("RocksDB did not create an OPTIONS file")
	}

	optionsData, err := os.ReadFile(optionFiles[len(optionFiles)-1])
	if err != nil {
		t.Fatalf("read latest RocksDB OPTIONS file: %v", err)
	}

	want := len(keys.BonsaiCFNames())
	if got := bytes.Count(optionsData, []byte("[CFOptions \"")); got != want {
		t.Fatalf("RocksDB OPTIONS has %d CF sections, want %d", got, want)
	}
	if got := bytes.Count(optionsData, []byte("filter_policy=bloomfilter")); got != want {
		t.Fatalf("RocksDB OPTIONS has %d Bloom-filter CFs, want %d", got, want)
	}
	if bytes.Contains(optionsData, []byte("filter_policy=nullptr")) {
		t.Fatal("RocksDB OPTIONS contains a column family without a filter policy")
	}
}
