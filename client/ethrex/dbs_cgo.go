//go:build cgo_ethrex

package ethrex

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"

	"github.com/linxGnu/grocksdb"

	ethrexinternal "github.com/nerolation/state-actor/internal/ethrex"
)

// bulkBackgroundJobs caps RocksDB's background compaction/flush thread pool
// during bulk import.
const bulkBackgroundJobs = 8

// ethrexBlockCacheBytes is the shared LRU block cache, mirroring ethrex's
// crates/storage/backend/rocksdb.rs (4 GiB, shared across all CFs).
const ethrexBlockCacheBytes = 4 * 1024 * 1024 * 1024

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
