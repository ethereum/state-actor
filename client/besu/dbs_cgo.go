//go:build cgo_besu

package besu

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/besu/keys"
)

// bulkBackgroundJobs caps RocksDB's background compaction/flush thread pool
// during bulk import. 8 is enough to drive the final CompactRange in parallel
// without ballooning RAM.
const bulkBackgroundJobs = 8

// perCFWriteBufferBytes is the per-CF memtable size during bulk import. Larger
// → fewer L0 SSTs but more RAM per CF.
const perCFWriteBufferBytes = 256 * 1024 * 1024

// Besu opens ONE RocksDB instance under <datadir>/database/ with all 17
// column families a fresh mainnet Bonsai init creates (keys.BonsaiCFNames).
// Even CFs that receive no genesis-time writes (TRIE_LOG_STORAGE, the
// backward-sync/snap-sync/legacy-privacy/chain-pruner CFs) must be declared on OpenDb —
// RocksDB compares the open's CF list against on-disk CFs and fails if any
// is missing on subsequent reopens — and creating them empty keeps the DB
// layout identical to one initialized by Besu itself.

// CF indices into besuDB.cfs. Must match keys.BonsaiCFNames() order
// (KeyValueSegmentIdentifier enum order); pinned by TestCFIndicesMatchNames.
const (
	cfIdxDefault = iota
	cfIdxBlockchain
	cfIdxPrivateTransactions
	cfIdxPrivateState
	cfIdxAccountInfoState
	cfIdxCodeStorage
	cfIdxAccountStorageStorage
	cfIdxTrieBranchStorage
	cfIdxTrieLogStorage
	cfIdxVariables
	cfIdxGoQuorumPrivateStorage
	cfIdxBackwardSyncHeaders
	cfIdxBackwardSyncBlocks
	cfIdxBackwardSyncChain
	cfIdxSnapsyncMissingAccountRange
	cfIdxSnapsyncAccountToFix
	cfIdxChainPrunerState
)

// besuDB holds the open grocksdb handle and the 17 CF handles. Caller closes
// via Close() when done.
type besuDB struct {
	db   *grocksdb.DB
	cfs  []*grocksdb.ColumnFamilyHandle
	path string

	// Held for Destroy() during Close. grocksdb requires explicit option-bag
	// cleanup or it leaks C++ allocations.
	dbOpts    *grocksdb.Options
	cfOpts    []*grocksdb.Options
	tableOpts []*grocksdb.BlockBasedTableOptions
}

// openBesuDB creates a fresh Besu Bonsai RocksDB at <datadir>/database/.
// Refuses to open into an existing dir: re-running on top of a partial run
// could leave genesis block keys, world-state sentinels, and chainHeadHash
// inconsistent, with Besu booting off whichever wrote last.
//
// We open with plain OpenDbColumnFamilies, not OpenOptimisticTransactionDb —
// the on-disk file format is identical (the optimistic-tx wrapper only adds
// in-memory conflict checking), so Besu's OptimisticTransactionDB boot reads
// what we write here.
func openBesuDB(datadir string) (*besuDB, error) {
	dbPath := filepath.Join(datadir, "database")

	// Fresh-dir precondition.
	if _, err := os.Stat(dbPath); err == nil {
		return nil, fmt.Errorf(
			"--db=%s already contains a Besu DB at database/. "+
				"Refusing to write into it: a partial previous run could leave the world-state "+
				"sentinels and chainHeadHash inconsistent. Pass --db= to a fresh path, or "+
				"`rm -rf %s` first.",
			datadir, datadir,
		)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("besu: stat %s: %w", dbPath, err)
	}

	if err := os.MkdirAll(datadir, 0o755); err != nil {
		return nil, fmt.Errorf("besu: mkdir datadir: %w", err)
	}

	// CF names use LITERAL bytes (0x01..0x12 for segment CFs, UTF-8 "default"
	// for the default CF) per KeyValueSegmentIdentifier.java:27-77. Order
	// must match the cfIdx* constants above.
	cfNames := keys.BonsaiCFNames()

	// Match Besu's per-CF settings from RocksDBColumnarKeyValueStorage.java:
	// LZ4 compression, block-based table format_version=5, 32KB blocks,
	// BloomFilter(10), dynamic-level compaction, BlobDB on BLOCKCHAIN +
	// TRIE_LOG_STORAGE only. Matching avoids "files look weird, Besu silently
	// re-tunes on first open" surprises.
	mkTable := func() *grocksdb.BlockBasedTableOptions {
		t := grocksdb.NewDefaultBlockBasedTableOptions()
		t.SetBlockSize(32 << 10) // 32 KB
		t.SetFormatVersion(5)
		// SetFilterPolicy takes ownership of the native policy. A fresh policy
		// is therefore required for every CF; reusing one leaves every CF after
		// the first with filter_policy=nullptr.
		t.SetFilterPolicy(grocksdb.NewBloomFilterFull(10))
		// SetPartitionFilters and SetCacheIndexAndFilterBlocks ARE exposed by
		// grocksdb v1.10.x (earlier comments here claimed otherwise). Both
		// stay at RocksDB defaults deliberately: the bulk path is write-only
		// and Besu re-tunes table options at first open. NOTE the default
		// means index/filter blocks live in per-SST table readers, outside
		// any cache bound, growing with SST count — the term the ethrex
		// writer now routes through its shared cache; porting that here is a
		// tracked follow-up.
		return t
	}

	cfOpts := make([]*grocksdb.Options, len(cfNames))
	tableOpts := make([]*grocksdb.BlockBasedTableOptions, len(cfNames))
	for i := range cfNames {
		opts := grocksdb.NewDefaultOptions()
		opts.SetCompression(grocksdb.LZ4Compression)
		opts.SetLevelCompactionDynamicLevelBytes(true)
		// Bulk-import tuning: 256 MiB memtables, L0 triggers pinned to
		// MaxInt32 (no auto-compaction during writes), one final
		// CompactRange in Close().
		opts.SetWriteBufferSize(perCFWriteBufferBytes)
		opts.SetMaxWriteBufferNumber(4)
		opts.SetLevel0FileNumCompactionTrigger(math.MaxInt32)
		opts.SetLevel0SlowdownWritesTrigger(math.MaxInt32)
		opts.SetLevel0StopWritesTrigger(math.MaxInt32)
		opts.SetMaxBytesForLevelBase(2 * 1024 * 1024 * 1024) // 2 GiB L1 target
		t := mkTable()
		opts.SetBlockBasedTableFactory(t)
		if i == cfIdxBlockchain || i == cfIdxTrieLogStorage {
			opts.EnableBlobFiles(true)
			opts.SetMinBlobSize(100)
			opts.SetBlobCompressionType(grocksdb.LZ4Compression)
			// Besu enables blob GC on TRIE_LOG_STORAGE only.
			if i == cfIdxTrieLogStorage {
				opts.EnableBlobGC(true)
			}
		}
		cfOpts[i] = opts
		tableOpts[i] = t
	}

	dbOpts := grocksdb.NewDefaultOptions()
	dbOpts.SetCreateIfMissing(true)
	dbOpts.SetCreateIfMissingColumnFamilies(true)
	dbOpts.SetMaxTotalWalSize(1 << 30) // 1 GB — Besu's default
	dbOpts.SetKeepLogFileNum(7)        // 1 week of daily rotation
	// Multiple background threads for parallel CF flushes + parallel
	// CompactRange at Close. Capped at bulkBackgroundJobs.
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
		// Free the option bags we'd otherwise have leaked.
		dbOpts.Destroy()
		for _, o := range cfOpts {
			o.Destroy()
		}
		for _, t := range tableOpts {
			t.Destroy()
		}
		return nil, fmt.Errorf("besu: open RocksDB at %s: %w", dbPath, err)
	}

	return &besuDB{
		db:        db,
		cfs:       cfHandles,
		path:      dbPath,
		dbOpts:    dbOpts,
		cfOpts:    cfOpts,
		tableOpts: tableOpts,
	}, nil
}

// Close releases all open grocksdb resources. Safe to call multiple times
// and on partially-initialized structs.
//
// Before closing, runs a full-range CompactRange on every user CF so the LSM
// tree is flat when Besu later opens this DB — pays the deferred cost from
// the bulk write's suppressed compactions, parallelised across MaxBackgroundJobs.
func (b *besuDB) Close() {
	if b.db != nil {
		// CompactRange the written CFs only. The Default CF and the
		// mainnet-parity CFs (legacy privacy, backward-sync, snap-sync,
		// chain-pruner)
		// are empty in our usage; the Blockchain CF receives few writes
		// at genesis and a CompactRange on it is near-instant.
		emptyRange := grocksdb.Range{Start: nil, Limit: nil}
		for _, idx := range []int{
			cfIdxAccountInfoState,
			cfIdxCodeStorage,
			cfIdxAccountStorageStorage,
			cfIdxTrieBranchStorage,
			cfIdxBlockchain,
			cfIdxVariables,
		} {
			if idx < len(b.cfs) && b.cfs[idx] != nil {
				b.db.CompactRangeCF(b.cfs[idx], emptyRange)
			}
		}
	}

	for _, h := range b.cfs {
		if h != nil {
			h.Destroy()
		}
	}
	b.cfs = nil

	if b.db != nil {
		b.db.Close()
		b.db = nil
	}

	for _, t := range b.tableOpts {
		if t != nil {
			t.Destroy()
		}
	}
	b.tableOpts = nil

	for _, o := range b.cfOpts {
		if o != nil {
			o.Destroy()
		}
	}
	b.cfOpts = nil

	if b.dbOpts != nil {
		b.dbOpts.Destroy()
		b.dbOpts = nil
	}
}

// put writes a key/value to the named CF. Fast path for single writes; bulk
// writes use writeBatch (see node_sink_cgo.go).
func (b *besuDB) put(cfIdx int, key, value []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	return b.db.PutCF(wo, b.cfs[cfIdx], key, value)
}

// putSync writes with sync=true. Used for the final chainHeadHash write to
// guarantee ordered durability — chainHeadHash MUST be the last on-disk write
// or a crash mid-write can leave Besu booting against partial state.
func (b *besuDB) putSync(cfIdx int, key, value []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.SetSync(true)
	return b.db.PutCF(wo, b.cfs[cfIdx], key, value)
}
