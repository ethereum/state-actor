//go:build cgo_ethrex

package ethrex

import (
	"fmt"
	"math"
	"os"
	"runtime"

	"github.com/linxGnu/grocksdb"

	ethrexinternal "github.com/nerolation/state-actor/internal/ethrex"
)

// bulkBackgroundJobs caps RocksDB's background compaction/flush thread pool
// during bulk import.
const bulkBackgroundJobs = 8

// perCFWriteBufferBytes is the per-CF memtable size during bulk import.
const perCFWriteBufferBytes = 256 * 1024 * 1024

// CF index constants into ethrexDB.cfs. Must match Tables order in
// internal/ethrex/constants.go.
const (
	cfIdxChainData            = 0
	cfIdxAccountCodes         = 1
	cfIdxAccountCodeMetadata  = 2
	cfIdxBodies               = 3
	cfIdxBlockNumbers         = 4
	cfIdxCanonicalBlockHashes = 5
	cfIdxHeaders              = 6
	cfIdxPendingBlocks        = 7
	cfIdxTransactionLocations = 8
	cfIdxReceiptsV2           = 9
	cfIdxSnapState            = 10
	cfIdxInvalidAncestors     = 11
	cfIdxAccountTrieNodes     = 12
	cfIdxStorageTrieNodes     = 13
	cfIdxFullsyncHeaders      = 14
	cfIdxAccountFlatKeyValue  = 15
	cfIdxStorageFlatKeyValue  = 16
	cfIdxMiscValues           = 17
	cfIdxExecutionWitnesses   = 18
	cfIdxBlockAccessLists     = 19
)

// ethrexDB holds the open grocksdb handle and the 20 CF handles.
type ethrexDB struct {
	db     *grocksdb.DB
	cfs    []*grocksdb.ColumnFamilyHandle
	path   string
	dbOpts *grocksdb.Options
	cfOpts []*grocksdb.Options
}

// openEthrexDB creates a fresh ethrex RocksDB at cfg.DBPath.
// Refuses to open into an existing directory: a partial previous run could
// leave genesis block keys and trie rows inconsistent, making ethrex silently
// boot off partial state.
//
// ethrex uses a plain RocksDB with the bytewise comparator on all CFs.
// Only the comparator is load-bearing per the spike; no BlobDB, no Bloom
// filters required (ethrex doesn't use block-based filter on these CFs in
// the genesis path).
func openEthrexDB(dbPath string) (*ethrexDB, error) {
	// Fresh-dir precondition: a missing or EMPTY directory is fine (callers and
	// tests routinely `mkdir -p` the datadir first, and t.TempDir() pre-creates
	// it). A NON-EMPTY directory is refused: a partial previous run could leave
	// trie rows and genesis keys inconsistent, and it mirrors ethrex's own
	// Store::new, which errors on a non-empty dir lacking metadata.json.
	if entries, err := os.ReadDir(dbPath); err == nil {
		if len(entries) > 0 {
			return nil, fmt.Errorf(
				"--db=%s already exists and is non-empty. "+
					"Refusing to write into it: a partial previous run could leave trie rows "+
					"and genesis block keys inconsistent. Pass --db= to a fresh/empty path, or "+
					"`rm -rf %s` first.",
				dbPath, dbPath,
			)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("ethrex: stat %s: %w", dbPath, err)
	}

	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		return nil, fmt.Errorf("ethrex: mkdir: %w", err)
	}

	// The 20 named ethrex CFs, plus RocksDB's implicit "default" CF appended
	// LAST (index 20). RocksDB always creates "default" on a fresh DB, and an
	// open call must account for every existing CF or it errors with "you have
	// to open all column families". Appending it keeps the cfIdx* constants
	// (0..19) aligned with Tables; cfs[20] (default) is created but never
	// written. Mirrors besu's explicit CFDefault inclusion.
	cfNames := make([]string, 0, len(ethrexinternal.Tables)+1)
	cfNames = append(cfNames, ethrexinternal.Tables...)
	cfNames = append(cfNames, "default")

	cfOpts := make([]*grocksdb.Options, len(cfNames))
	for i := range cfNames {
		opts := grocksdb.NewDefaultOptions()
		// No compression — ethrex doesn't configure LZ4 in the genesis store
		// (default = no compression). Bulk-import tuning matches besu pattern.
		opts.SetWriteBufferSize(perCFWriteBufferBytes)
		opts.SetMaxWriteBufferNumber(4)
		opts.SetLevel0FileNumCompactionTrigger(math.MaxInt32)
		opts.SetLevel0SlowdownWritesTrigger(math.MaxInt32)
		opts.SetLevel0StopWritesTrigger(math.MaxInt32)
		cfOpts[i] = opts
	}

	dbOpts := grocksdb.NewDefaultOptions()
	dbOpts.SetCreateIfMissing(true)
	dbOpts.SetCreateIfMissingColumnFamilies(true)

	parallelism := runtime.NumCPU()
	if parallelism > bulkBackgroundJobs {
		parallelism = bulkBackgroundJobs
	}
	dbOpts.IncreaseParallelism(parallelism)
	dbOpts.SetMaxBackgroundJobs(parallelism)

	db, cfHandles, err := grocksdb.OpenDbColumnFamilies(
		dbOpts, dbPath, cfNames, cfOpts,
	)
	if err != nil {
		dbOpts.Destroy()
		for _, o := range cfOpts {
			o.Destroy()
		}
		return nil, fmt.Errorf("ethrex: open RocksDB at %s: %w", dbPath, err)
	}

	return &ethrexDB{
		db:     db,
		cfs:    cfHandles,
		path:   dbPath,
		dbOpts: dbOpts,
		cfOpts: cfOpts,
	}, nil
}

// Close flushes, compacts, and releases all open grocksdb resources.
// Runs a CompactRange on the written CFs before closing so the LSM tree is
// flat when ethrex opens the DB.
func (d *ethrexDB) Close() {
	if d.db != nil {
		emptyRange := grocksdb.Range{Start: nil, Limit: nil}
		for _, idx := range []int{
			cfIdxAccountTrieNodes,
			cfIdxStorageTrieNodes,
			cfIdxAccountCodes,
			cfIdxAccountCodeMetadata,
			cfIdxChainData,
			cfIdxHeaders,
			cfIdxBodies,
			cfIdxBlockNumbers,
			cfIdxCanonicalBlockHashes,
		} {
			if idx < len(d.cfs) && d.cfs[idx] != nil {
				d.db.CompactRangeCF(d.cfs[idx], emptyRange)
			}
		}
	}

	for _, h := range d.cfs {
		if h != nil {
			h.Destroy()
		}
	}
	d.cfs = nil

	if d.db != nil {
		d.db.Close()
		d.db = nil
	}

	for _, o := range d.cfOpts {
		if o != nil {
			o.Destroy()
		}
	}
	d.cfOpts = nil

	if d.dbOpts != nil {
		d.dbOpts.Destroy()
		d.dbOpts = nil
	}
}

// put writes a key/value to the CF at cfIdx.
func (d *ethrexDB) put(cfIdx int, key, value []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	return d.db.PutCF(wo, d.cfs[cfIdx], key, value)
}

// putSync writes with sync=true. Used for the canonical_block_hashes boot-gate
// write that must be the last durable write.
func (d *ethrexDB) putSync(cfIdx int, key, value []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.SetSync(true)
	return d.db.PutCF(wo, d.cfs[cfIdx], key, value)
}
