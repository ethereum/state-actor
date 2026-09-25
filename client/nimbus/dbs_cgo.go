//go:build cgo_nimbus

package nimbus

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/memstat"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

// Column-family handle indexes into nimbusDB.cfs — the order of
// internal/nimbus.ColumnFamilies.
const (
	cfIdxDefault = 0
	cfIdxAriVtx  = 1
	cfIdxKvtGen  = 2
	cfIdxKvtSync = 3
)

// bulkBackgroundJobs caps RocksDB's background compaction/flush thread pool
// during bulk import.
const bulkBackgroundJobs = 8

// defaultLevelBaseMiB is max_bytes_for_level_base for AriVtx and KvtGen:
// 2 GiB, the value the besu / nethermind / ethrex writers converged on (see
// client/ethrex/dbs_cgo.go for the ladder). Without it RocksDB's 256 MB
// default compacts L0 continuously through the import despite the maxed L0
// triggers. STATE_ACTOR_NIMBUS_LEVELBASE_MIB overrides for experiments.
const defaultLevelBaseMiB = 2048

func nimbusLevelBaseBytes() uint64 {
	if v := os.Getenv("STATE_ACTOR_NIMBUS_LEVELBASE_MIB"); v != "" {
		if mib, err := strconv.ParseUint(v, 10, 64); err == nil && mib > 0 {
			return mib << 20
		}
		log.Printf("nimbus: ignoring invalid STATE_ACTOR_NIMBUS_LEVELBASE_MIB=%q", v)
	}
	return defaultLevelBaseMiB << 20
}

// Off-heap budget (see doc.go "Memory" and the ethrex writer's derivation).
const (
	// nimbusBlockCacheBytes is the shared LRU block cache; with
	// cache_index_and_filter_blocks it also bounds index + filter blocks.
	nimbusBlockCacheBytes = 512 * 1024 * 1024
	// nimbusDBWriteBufferBytes caps total memtable memory: AriVtx 256 MiB × 4
	// plus KvtGen 128 MiB × 3 plus the two idle CFs sum below this backstop.
	nimbusDBWriteBufferBytes = 2 * 1024 * 1024 * 1024
	// nimbusMaxOpenFiles is a table-cache backstop set where it cannot bind
	// (L0 accumulates for the whole import; Close needs every input open).
	nimbusMaxOpenFiles = 32768
	// nimbusAuxOffHeapBytes estimates WriteBatches (shared + per-worker),
	// the streamsort Pebble arenas and compaction scratch.
	nimbusAuxOffHeapBytes = 2 * 1024 * 1024 * 1024
	// nimbusAllocatorSlackBytes covers jemalloc retention over the C heap.
	nimbusAllocatorSlackBytes = 1536 * 1024 * 1024
	// nimbusOffHeapReserveBytes is what runImpl hands to memlimit.Set.
	nimbusOffHeapReserveBytes = nimbusBlockCacheBytes + nimbusDBWriteBufferBytes +
		nimbusAuxOffHeapBytes + nimbusAllocatorSlackBytes
)

// nimbusDB holds the open grocksdb handle and the four CF handles.
type nimbusDB struct {
	db     *grocksdb.DB
	cfs    []*grocksdb.ColumnFamilyHandle
	path   string
	dbOpts *grocksdb.Options
	cfOpts []*grocksdb.Options
	cache  *grocksdb.Cache
	bbtos  []*grocksdb.BlockBasedTableOptions
}

// openNimbusDB creates a fresh nimbus RocksDB at ecdbDir (<db>/ecdb).
//
// Options that shape the final bytes mirror nimbus's own CF options
// (execution_chain/db/core_db/backend/aristo_rocksdb.nim toCfOpts):
// no per-level compression with ZSTD at the bottommost level, a 9.9-bit
// ribbon-hybrid filter, two-level partitioned index + filters, the
// binary-search-and-hash data-block index at ratio 0.75, 16 KiB blocks,
// target_file_size_base 64 MiB (×4 per level) and a level multiplier of 16.
// Nimbus's librocksdb is built with lz4 + zstd only, so every CF sets its
// compression explicitly — RocksDB's Snappy default would be unreadable.
// Deliberate deviations are process-runtime knobs that cannot change the
// compacted output: L0 triggers maxed for bulk import, 256 MiB memtables, the
// 2 GiB level base and the shared 512 MiB cache. Close() runs a KForce
// CompactRange so the shipped SSTs carry the per-CF options above.
func openNimbusDB(ecdbDir string) (*nimbusDB, error) {
	if entries, err := os.ReadDir(ecdbDir); err == nil {
		if len(entries) > 0 {
			return nil, fmt.Errorf(
				"--db=%s already holds a nimbus database (%s is non-empty). "+
					"Refusing to write into it: a partial previous run could leave trie rows "+
					"and the genesis rows inconsistent. Pass --db= to a fresh/empty path, or "+
					"`rm -rf %s` first.",
				ecdbDir, ecdbDir, ecdbDir,
			)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("nimbus: stat %s: %w", ecdbDir, err)
	}
	if err := os.MkdirAll(ecdbDir, 0o755); err != nil {
		return nil, fmt.Errorf("nimbus: mkdir: %w", err)
	}

	cfNames := nimbusinternal.ColumnFamilies
	cache := grocksdb.NewLRUCache(nimbusBlockCacheBytes)

	cfOpts := make([]*grocksdb.Options, len(cfNames))
	bbtos := make([]*grocksdb.BlockBasedTableOptions, len(cfNames))
	for i := range cfNames {
		opts := grocksdb.NewDefaultOptions()
		// Never Snappy (nimbus cannot read it); ZSTD only where nimbus puts it.
		opts.SetCompression(grocksdb.NoCompression)
		opts.SetBottommostCompression(grocksdb.ZSTDCompression)
		opts.SetLevel0FileNumCompactionTrigger(math.MaxInt32)
		opts.SetLevel0SlowdownWritesTrigger(math.MaxInt32)
		opts.SetLevel0StopWritesTrigger(math.MaxInt32)

		bbto := grocksdb.NewDefaultBlockBasedTableOptions()
		bbto.SetBlockCache(cache)
		bbto.SetCacheIndexAndFilterBlocks(true)
		bbto.SetBlockSize(16 << 10)

		switch i {
		case cfIdxAriVtx, cfIdxKvtGen:
			if i == cfIdxAriVtx {
				opts.SetWriteBufferSize(256 << 20)
				opts.SetMaxWriteBufferNumber(4)
			} else {
				opts.SetWriteBufferSize(128 << 20)
				opts.SetMaxWriteBufferNumber(3)
			}
			opts.SetMaxBytesForLevelBase(nimbusLevelBaseBytes())
			opts.SetMaxBytesForLevelMultiplier(16)
			opts.SetTargetFileSizeBase(64 << 20)
			opts.SetTargetFileSizeMultiplier(4)
			// nimbus toCfOpts: ribbon-hybrid filter, partitioned two-level
			// index + filters with the top level pinned, hash data-block index.
			bbto.SetFilterPolicy(grocksdb.NewRibbonHybridFilterPolicy(9.9, 0))
			bbto.SetIndexType(grocksdb.KTwoLevelIndexSearchIndexType)
			bbto.SetPartitionFilters(true)
			bbto.SetPinTopLevelIndexAndFilter(true)
			bbto.SetCacheIndexAndFilterBlocksWithHighPriority(true)
			bbto.SetDataBlockIndexType(grocksdb.KDataBlockIndexTypeBinarySearchAndHash)
			bbto.SetDataBlockHashRatio(0.75)
		default:
			opts.SetWriteBufferSize(64 << 20)
			opts.SetMaxWriteBufferNumber(2)
		}

		opts.SetBlockBasedTableFactory(bbto)
		cfOpts[i] = opts
		bbtos[i] = bbto
	}

	dbOpts := grocksdb.NewDefaultOptions()
	dbOpts.SetCreateIfMissing(true)
	dbOpts.SetCreateIfMissingColumnFamilies(true)
	dbOpts.SetDbWriteBufferSize(nimbusDBWriteBufferBytes)
	dbOpts.SetMaxOpenFiles(nimbusMaxOpenFiles)
	parallelism := runtime.NumCPU()
	if parallelism > bulkBackgroundJobs {
		parallelism = bulkBackgroundJobs
	}
	dbOpts.IncreaseParallelism(parallelism)
	// nimbus sets max_subcompactions = max_background_jobs; it lets Close()'s
	// L0 → base merge fan out.
	dbOpts.SetMaxSubcompactions(uint32(parallelism))

	db, cfHandles, err := grocksdb.OpenDbColumnFamilies(dbOpts, ecdbDir, cfNames, cfOpts)
	if err != nil {
		dbOpts.Destroy()
		for _, o := range cfOpts {
			o.Destroy()
		}
		for _, b := range bbtos {
			b.Destroy()
		}
		cache.Destroy()
		return nil, fmt.Errorf("nimbus: open RocksDB at %s: %w", ecdbDir, err)
	}
	return &nimbusDB{db: db, cfs: cfHandles, path: ecdbDir, dbOpts: dbOpts, cfOpts: cfOpts, cache: cache, bbtos: bbtos}, nil
}

// Close compacts the written CFs and releases every grocksdb resource.
// bottommost_level_compaction=KForce is load-bearing: the default merely
// moves L0 files into the empty bottom level, leaving files nimbus's first
// open would mark for compaction and rewrite wholesale. Serial, largest CF
// first, so an interrupt leaves the cheap CF uncompacted rather than AriVtx.
func (d *nimbusDB) Close() {
	if d.db != nil {
		emptyRange := grocksdb.Range{Start: nil, Limit: nil}
		cro := grocksdb.NewCompactRangeOptions()
		defer cro.Destroy()
		cro.SetBottommostLevelCompaction(grocksdb.KForce)
		for _, idx := range []int{cfIdxAriVtx, cfIdxKvtGen} {
			if idx < len(d.cfs) && d.cfs[idx] != nil {
				start := time.Now()
				d.db.CompactRangeCFOpt(d.cfs[idx], emptyRange, cro)
				log.Printf("  nimbus: compacted %s in %s · mem %s · %s",
					nimbusinternal.ColumnFamilies[idx], time.Since(start).Round(time.Second),
					memstat.Read(), d.memoryReport())
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
	// Shared by every CF table factory — free last.
	if d.cache != nil {
		d.cache.Destroy()
		d.cache = nil
	}
}

// memoryReport renders RocksDB's own accounting for the two data CFs (see
// the ethrex writer's memoryReport for what each term means).
func (d *nimbusDB) memoryReport() string {
	dataCFs := []int{cfIdxAriVtx, cfIdxKvtGen}
	sum := func(prop string) uint64 {
		var total uint64
		for _, idx := range dataCFs {
			if idx < len(d.cfs) && d.cfs[idx] != nil {
				if v, ok := d.db.GetIntPropertyCF(prop, d.cfs[idx]); ok {
					total += v
				}
			}
		}
		return total
	}
	var cacheUsage, cachePinned uint64
	if len(d.cfs) > cfIdxAriVtx && d.cfs[cfIdxAriVtx] != nil {
		cacheUsage, _ = d.db.GetIntPropertyCF("rocksdb.block-cache-usage", d.cfs[cfIdxAriVtx])
		cachePinned, _ = d.db.GetIntPropertyCF("rocksdb.block-cache-pinned-usage", d.cfs[cfIdxAriVtx])
	}
	var l0 uint64
	for _, idx := range dataCFs {
		if idx < len(d.cfs) && d.cfs[idx] != nil {
			v := d.db.GetPropertyCF("rocksdb.num-files-at-level0", d.cfs[idx])
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				l0 += n
			}
		}
	}
	return fmt.Sprintf("rocksdb memtables=%s table-readers=%s cache=%s cache-pinned=%s L0-files=%d",
		memstat.FormatBytes(sum("rocksdb.cur-size-all-mem-tables")),
		memstat.FormatBytes(sum("rocksdb.estimate-table-readers-mem")),
		memstat.FormatBytes(cacheUsage), memstat.FormatBytes(cachePinned), l0)
}

// put writes one row (WAL on).
func (d *nimbusDB) put(cfIdx int, key, value []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	return d.db.PutCF(wo, d.cfs[cfIdx], key, value)
}

// flushData forces the AriVtx and KvtGen memtables to SST and waits. The bulk
// vertex and code rows are written with the WAL disabled, so until this runs
// they live only in memory — a later sync write does not make them durable.
func (d *nimbusDB) flushData() error {
	fo := grocksdb.NewDefaultFlushOptions()
	defer fo.Destroy()
	fo.SetWait(true)
	cfs := []*grocksdb.ColumnFamilyHandle{d.cfs[cfIdxAriVtx], d.cfs[cfIdxKvtGen]}
	return d.db.FlushCFs(cfs, fo)
}

// putSync writes one row with sync=true — for the admin record and the
// canonical-head boot gate, which must be the last durable writes.
func (d *nimbusDB) putSync(cfIdx int, key, value []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	wo.SetSync(true)
	return d.db.PutCF(wo, d.cfs[cfIdx], key, value)
}
