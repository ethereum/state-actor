//go:build cgo_ethrex

package ethrex

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"

	"github.com/linxGnu/grocksdb"

	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
	"github.com/ethereum/state-actor/internal/memstat"
)

// bulkBackgroundJobs caps RocksDB's background compaction/flush thread pool
// during bulk import.
const bulkBackgroundJobs = 8

// ethrexBlockCacheBytes is the shared LRU block cache, mirroring ethrex's
// crates/storage/backend/rocksdb.rs (4 GiB, shared across all CFs).
//
// Paired with cache_index_and_filter_blocks (set per-CF below), this is also
// the ceiling for index and bloom-filter blocks. That pairing is the point:
// RocksDB's default pre-loads them into each table reader instead, outside
// every budget and unevictable for as long as the SST stays open. At the
// scale this writer targets — billions of keys across the four big state CFs,
// with 10-bit filters and 131-byte storage_flatkeyvalue keys — that is several
// GiB of RAM that grows monotonically with every SST written, and it is one
// of the terms that OOM-killed a 350 GB ethrex run mid-import.
const ethrexBlockCacheBytes = 4 * 1024 * 1024 * 1024

// ethrexDBWriteBufferBytes caps TOTAL memtable memory across all CFs.
//
// The per-CF write buffers below mirror ethrex, but they are per-CF ceilings
// that RocksDB does not sum: the four big state CFs alone would permit
// 512 MiB x 6 = 12 GiB resident. db_write_buffer_size bounds the sum, flushing
// the largest memtable when the budget is exceeded. Keeping the per-CF shape
// intact matters — it governs flushed SST sizes, and Close()'s CompactRange
// rewrites with the same per-CF options either way.
const ethrexDBWriteBufferBytes = 4 * 1024 * 1024 * 1024

// ethrexMaxOpenFiles is a backstop on RocksDB's table cache, deliberately set
// where it cannot bind. With L0 compaction triggers pinned to MaxInt32 (below)
// SSTs accumulate for the whole import — a 700 GB DB at ~512 MiB per flushed
// file is ~1400 of them, and the db_write_buffer_size cap can only shrink
// files, not enlarge them. Sizing this an order of magnitude above that keeps
// residual per-table-reader overhead bounded (a handle plus footer metadata,
// once cache_index_and_filter_blocks moves the expensive part into the cache)
// without risking reopen churn during Close()'s CompactRange, which needs
// every input file open at once. RLIMIT_NOFILE is not the constraint: CI
// raises it to the kernel max before the container starts.
const ethrexMaxOpenFiles = 32768

// ethrexAuxOffHeapBytes is an ESTIMATE (not a bound) of the off-heap RAM this
// writer commits beyond the two budgets above: the batchSink WriteBatches
// (six in Phase 2; up to 8 workers x 2 in Phase 0, each flushing at 64 MiB),
// the Pebble streamsort store's two 256 MiB memtable arenas, and RocksDB's
// compaction/iterator scratch.
const ethrexAuxOffHeapBytes = 2 * 1024 * 1024 * 1024

// ethrexOffHeapReserveBytes is the total RAM this writer commits OUTSIDE the
// Go heap. runImpl subtracts it from the host's memory ceiling before deriving
// GOMEMLIMIT, so the Go runtime budgets against what is actually left rather
// than against memory RocksDB and Pebble have already claimed.
const ethrexOffHeapReserveBytes = ethrexBlockCacheBytes +
	ethrexDBWriteBufferBytes +
	ethrexAuxOffHeapBytes

// ethrexCompressibleCF reports whether a CF uses LZ4 in the real ethrex client.
// Mirrors `compressible_tables` in ethrex rocksdb.rs: only block-metadata CFs
// are LZ4; the big state CFs (trie nodes, flat KV, codes) are uncompressed.
func ethrexCompressibleCF(cfIdx int) bool {
	switch cfIdx {
	case cfIdxBlockNumbers, cfIdxHeaders, cfIdxBodies, cfIdxReceiptsV2,
		cfIdxTransactionLocations, cfIdxFullsyncHeaders:
		return true
	default:
		return false
	}
}

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
	cache  *grocksdb.Cache                    // shared block cache; freed last
	bbtos  []*grocksdb.BlockBasedTableOptions // per-CF table options; freed at Close
}

// openEthrexDB creates a fresh ethrex RocksDB at cfg.DBPath.
// Refuses to open into an existing directory: a partial previous run could
// leave genesis block keys and trie rows inconsistent, making ethrex silently
// boot off partial state.
//
// Per-CF options (compression, block size, bloom filters, write buffers, blob
// files, shared 4 GiB block cache) mirror the real ethrex client's
// crates/storage/backend/rocksdb.rs exactly, so a state-actor-produced DB is
// byte-representative for benchmarking. The only deliberate deviation is the
// L0 compaction triggers: ethrex uses 4/20/36, state-actor maxes them to avoid
// compaction stalls during bulk import — Close() runs CompactRange afterward,
// which rewrites every SST with these same compression/block/bloom options, so
// the final on-disk shape matches ethrex regardless.
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

	// Shared 4 GiB LRU block cache across all CFs (ethrex rocksdb.rs).
	cache := grocksdb.NewLRUCache(ethrexBlockCacheBytes)

	cfOpts := make([]*grocksdb.Options, len(cfNames))
	bbtos := make([]*grocksdb.BlockBasedTableOptions, len(cfNames))
	for i := range cfNames {
		opts := grocksdb.NewDefaultOptions()

		// Compression: LZ4 on block-metadata CFs, None on the big state CFs —
		// exactly ethrex's compressible_tables split.
		if ethrexCompressibleCF(i) {
			opts.SetCompression(grocksdb.LZ4Compression)
		} else {
			opts.SetCompression(grocksdb.NoCompression)
		}

		// L0 triggers maxed for bulk import (Close() CompactRange flattens the
		// LSM with the per-CF options below, so the final shape still matches).
		opts.SetLevel0FileNumCompactionTrigger(math.MaxInt32)
		opts.SetLevel0SlowdownWritesTrigger(math.MaxInt32)
		opts.SetLevel0StopWritesTrigger(math.MaxInt32)

		bbto := grocksdb.NewDefaultBlockBasedTableOptions()
		bbto.SetBlockCache(cache)
		// Route index + filter blocks through the shared cache instead of
		// pre-loading them into per-SST table readers. This is a read-path
		// memory-placement option only: it changes nothing about the bytes
		// written, and nothing is read during bulk import, so eviction costs
		// nothing here. See ethrexBlockCacheBytes for why it is load-bearing.
		bbto.SetCacheIndexAndFilterBlocks(true)

		switch i {
		case cfIdxHeaders, cfIdxBodies:
			opts.SetWriteBufferSize(128 << 20)
			opts.SetMaxWriteBufferNumber(4)
			opts.SetTargetFileSizeBase(256 << 20)
			bbto.SetBlockSize(32 << 10)
		case cfIdxCanonicalBlockHashes, cfIdxBlockNumbers:
			opts.SetWriteBufferSize(64 << 20)
			opts.SetMaxWriteBufferNumber(3)
			opts.SetTargetFileSizeBase(128 << 20)
			bbto.SetBlockSize(16 << 10)
			bbto.SetFilterPolicy(grocksdb.NewBloomFilterFull(10))
		case cfIdxAccountTrieNodes, cfIdxStorageTrieNodes,
			cfIdxAccountFlatKeyValue, cfIdxStorageFlatKeyValue:
			opts.SetWriteBufferSize(512 << 20)
			opts.SetMaxWriteBufferNumber(6)
			opts.SetMinWriteBufferNumberToMerge(2)
			opts.SetTargetFileSizeBase(256 << 20)
			opts.SetMemTablePrefixBloomSizeRatio(0.2)
			bbto.SetBlockSize(16 << 10)
			bbto.SetFilterPolicy(grocksdb.NewBloomFilterFull(10))
		case cfIdxAccountCodes:
			opts.SetWriteBufferSize(128 << 20)
			opts.SetMaxWriteBufferNumber(3)
			opts.SetTargetFileSizeBase(256 << 20)
			// Bytecodes go to blob files; small ones (delegation indicators)
			// stay inline. Blobs are LZ4-compressed.
			opts.EnableBlobFiles(true)
			opts.SetMinBlobSize(32)
			opts.SetBlobCompressionType(grocksdb.LZ4Compression)
			bbto.SetBlockSize(32 << 10)
		case cfIdxReceiptsV2:
			opts.SetWriteBufferSize(128 << 20)
			opts.SetMaxWriteBufferNumber(3)
			opts.SetTargetFileSizeBase(256 << 20)
			bbto.SetBlockSize(32 << 10)
		default:
			opts.SetWriteBufferSize(64 << 20)
			opts.SetMaxWriteBufferNumber(3)
			opts.SetTargetFileSizeBase(128 << 20)
			bbto.SetBlockSize(16 << 10)
		}

		opts.SetBlockBasedTableFactory(bbto)
		cfOpts[i] = opts
		bbtos[i] = bbto
	}

	dbOpts := grocksdb.NewDefaultOptions()
	dbOpts.SetCreateIfMissing(true)
	dbOpts.SetCreateIfMissingColumnFamilies(true)
	// Memory ceilings. Neither affects the produced SSTs — db_write_buffer_size
	// changes how often memtables flush (and Close()'s CompactRange rewrites
	// the result at target_file_size_base regardless), max_open_files only
	// governs the table cache.
	dbOpts.SetDbWriteBufferSize(ethrexDBWriteBufferBytes)
	dbOpts.SetMaxOpenFiles(ethrexMaxOpenFiles)

	parallelism := runtime.NumCPU()
	if parallelism > bulkBackgroundJobs {
		parallelism = bulkBackgroundJobs
	}
	// IncreaseParallelism already sets max background jobs to the same value;
	// no separate SetMaxBackgroundJobs needed.
	dbOpts.IncreaseParallelism(parallelism)

	db, cfHandles, err := grocksdb.OpenDbColumnFamilies(
		dbOpts, dbPath, cfNames, cfOpts,
	)
	if err != nil {
		dbOpts.Destroy()
		for _, o := range cfOpts {
			o.Destroy()
		}
		for _, b := range bbtos {
			if b != nil {
				b.Destroy()
			}
		}
		cache.Destroy()
		return nil, fmt.Errorf("ethrex: open RocksDB at %s: %w", dbPath, err)
	}

	return &ethrexDB{
		db:     db,
		cfs:    cfHandles,
		path:   dbPath,
		dbOpts: dbOpts,
		cfOpts: cfOpts,
		cache:  cache,
		bbtos:  bbtos,
	}, nil
}

// Close flushes, compacts, and releases all open grocksdb resources.
// Runs a CompactRange on the written CFs before closing so the LSM tree is
// flat when ethrex opens the DB. The CFs are independent, so the compactions
// run concurrently — RocksDB schedules them onto the shared background-job
// pool (max_background_jobs), turning a serial CF-by-CF wait into a parallel
// one. This is the dominant cost at large DB sizes, where the bulk write
// phase is I/O/compaction-bound rather than CPU-bound.
func (d *ethrexDB) Close() {
	if d.db != nil {
		emptyRange := grocksdb.Range{Start: nil, Limit: nil}
		// exclusive=false lets these per-CF manual compactions overlap instead
		// of serializing on RocksDB's exclusive-manual-compaction lock; the
		// shared CompactRangeOptions is read-only during compaction so it is
		// safe to use from all goroutines.
		cro := grocksdb.NewCompactRangeOptions()
		cro.SetExclusiveManualCompaction(false)
		var wg sync.WaitGroup
		for _, idx := range []int{
			cfIdxAccountTrieNodes,
			cfIdxStorageTrieNodes,
			cfIdxAccountFlatKeyValue,
			cfIdxStorageFlatKeyValue,
			cfIdxAccountCodes,
			cfIdxAccountCodeMetadata,
			cfIdxChainData,
			cfIdxMiscValues,
			cfIdxHeaders,
			cfIdxBodies,
			cfIdxBlockNumbers,
			cfIdxCanonicalBlockHashes,
		} {
			if idx < len(d.cfs) && d.cfs[idx] != nil {
				wg.Add(1)
				go func(cf *grocksdb.ColumnFamilyHandle) {
					defer wg.Done()
					d.db.CompactRangeCFOpt(cf, emptyRange, cro)
				}(d.cfs[idx])
			}
		}
		wg.Wait()
		cro.Destroy()
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

	for _, b := range d.bbtos {
		if b != nil {
			b.Destroy()
		}
	}
	d.bbtos = nil

	if d.dbOpts != nil {
		d.dbOpts.Destroy()
		d.dbOpts = nil
	}

	// Cache is shared by all CF table factories — free it LAST, after the DB
	// and all options that reference it are gone.
	if d.cache != nil {
		d.cache.Destroy()
		d.cache = nil
	}
}

// stateCFsForMemoryReport are the CFs whose memory is worth attributing
// individually: the four that carry essentially all of a bulk import's bytes.
var stateCFsForMemoryReport = []int{
	cfIdxAccountTrieNodes,
	cfIdxStorageTrieNodes,
	cfIdxAccountFlatKeyValue,
	cfIdxStorageFlatKeyValue,
}

// memoryReport renders RocksDB's own accounting of where its memory has gone.
//
// This exists because the budget constants in this file are ceilings, not
// measurements: they say what RocksDB is ALLOWED to use, and an OOM means
// something exceeded an estimate rather than a bound. These properties are
// RocksDB's ground truth.
//
//   - memtables: charged against ethrexDBWriteBufferBytes.
//   - table-readers: per-SST index/filter/metadata. cache_index_and_filter_blocks
//     should keep the expensive part in the cache and this number small; if it
//     grows with the import instead, that setting is not covering what it
//     should.
//   - cache / pinned: usage of the shared ethrexBlockCacheBytes LRU. An LRU is
//     not a hard bound — pinned entries can push usage past capacity — so
//     cache exceeding its capacity is itself a finding.
//   - L0 files: the SST count, since nothing compacts them during the import.
func (d *ethrexDB) memoryReport() string {
	sum := func(prop string) uint64 {
		var total uint64
		for _, idx := range stateCFsForMemoryReport {
			if idx >= len(d.cfs) || d.cfs[idx] == nil {
				continue
			}
			if v, ok := d.db.GetIntPropertyCF(prop, d.cfs[idx]); ok {
				total += v
			}
		}

		return total
	}

	// Block-cache usage is a property of the shared cache, so it reads the
	// same from every CF — take it from one rather than summing four copies.
	var cacheUsage, cachePinned uint64
	if len(d.cfs) > cfIdxAccountTrieNodes && d.cfs[cfIdxAccountTrieNodes] != nil {
		cacheUsage, _ = d.db.GetIntPropertyCF("rocksdb.block-cache-usage", d.cfs[cfIdxAccountTrieNodes])
		cachePinned, _ = d.db.GetIntPropertyCF("rocksdb.block-cache-pinned-usage", d.cfs[cfIdxAccountTrieNodes])
	}

	return fmt.Sprintf(
		"rocksdb memtables=%s table-readers=%s cache=%s cache-pinned=%s L0-files=%d",
		memstat.FormatBytes(sum("rocksdb.cur-size-all-mem-tables")),
		memstat.FormatBytes(sum("rocksdb.estimate-table-readers-mem")),
		memstat.FormatBytes(cacheUsage),
		memstat.FormatBytes(cachePinned),
		sum("rocksdb.num-files-at-level0"),
	)
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
