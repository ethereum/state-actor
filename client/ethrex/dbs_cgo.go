//go:build cgo_ethrex

package ethrex

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

	ethrexinternal "github.com/ethereum/state-actor/internal/ethrex"
	"github.com/ethereum/state-actor/internal/memstat"
)

// bulkBackgroundJobs caps RocksDB's background compaction/flush thread pool
// during bulk import.
const bulkBackgroundJobs = 8

// defaultStateCFLevelBaseMiB is max_bytes_for_level_base for the four state
// CFs, in MiB: 2 GiB, matching besu and nethermind.
//
// Ladder-measured (40 GB target, 96-core host, seed 42, identical roots):
// RocksDB's 256 MB default wrote 314 GiB physical in 20.8 min; 2 GiB wrote
// 298 GiB in 15.8 min; the defer-everything variant (1 TiB) wrote the least
// (272 GiB) but took 19.5 min — its deferred flattening lands in Close as a
// serial ~400-input merge per state CF. 2 GiB wins on wall by keeping cheap
// incremental L0 passes overlapped with import while Close stays ~35 s/CF.
// STATE_ACTOR_ETHREX_LEVELBASE_MIB overrides for experiments.
const defaultStateCFLevelBaseMiB = 2048

// ethrexStateCFLevelBaseBytes resolves the state-CF level-base, honoring
// the STATE_ACTOR_ETHREX_LEVELBASE_MIB env override (benchmarking knob,
// same pattern as erigon's STATE_ACTOR_ERIGON_WORKERS).
func ethrexStateCFLevelBaseBytes() uint64 {
	if v := os.Getenv("STATE_ACTOR_ETHREX_LEVELBASE_MIB"); v != "" {
		if mib, err := strconv.ParseUint(v, 10, 64); err == nil && mib > 0 {
			return mib << 20
		}
		log.Printf("ethrex: ignoring invalid STATE_ACTOR_ETHREX_LEVELBASE_MIB=%q", v)
	}
	return defaultStateCFLevelBaseMiB << 20
}

// ethrexBlockCacheBytes is the shared LRU block cache across all CFs.
//
// ethrex (crates/storage/backend/rocksdb.rs) defaults to 12 GiB
// (DEFAULT_ROCKSDB_BLOCK_CACHE_SIZE_BYTES, overridable with
// --rocksdb.block-cache-size). This writer uses far less BY DESIGN: a block
// cache only accelerates reads, and generation is
// write-only until Close()'s CompactRange. Cache size is a process-runtime
// knob — it does not change a single byte of the produced DB — so mirroring
// ethrex here would buy representativeness that does not exist while costing
// RAM that demonstrably does.
//
// Paired with cache_index_and_filter_blocks (set per-CF below) it is also the
// ceiling for index and bloom-filter blocks. RocksDB's default pre-loads those
// into each table reader instead, outside every budget. Measurement says the
// pairing works and the sizing is safe: over a 79 GB fill,
// estimate-table-readers-mem stayed at 388 KB while cache usage climbed to
// 1.5 GiB — so index/filter memory is bounded here, and capping the cache
// merely evicts blocks nothing is reading.
const ethrexBlockCacheBytes = 512 * 1024 * 1024

// ethrexDBWriteBufferBytes caps TOTAL memtable memory across all CFs.
//
// The four state CFs are sized at 256 MiB × 4 (see the state-CF case), which
// sums to exactly this cap — in the steady state the per-CF sizes are the
// operative flush trigger and this is the backstop for the long tail of
// smaller CFs pushing the total over. RocksDB does not sum per-CF ceilings on
// its own; without this cap the previous 512 MiB × 6 shape permitted 12 GiB
// resident. Flush cadence is process-runtime only — Close()'s CompactRange
// rewrites the result at target_file_size_base either way.
const ethrexDBWriteBufferBytes = 4 * 1024 * 1024 * 1024

// ethrexMaxOpenFiles is a backstop on RocksDB's table cache, deliberately set
// where it cannot bind. With L0 compaction triggers pinned to MaxInt32 (below)
// SSTs accumulate for the whole import — a 700 GB DB at ~256 MiB per flushed
// file is ~2800 of them, and the db_write_buffer_size cap can only shrink
// files, not enlarge them. Sizing this more than 10× above that keeps
// residual per-table-reader overhead bounded (a handle plus footer metadata,
// once cache_index_and_filter_blocks moves the expensive part into the cache)
// without risking reopen churn during Close()'s CompactRange, which needs
// every input file open at once. RLIMIT_NOFILE is not the constraint: CI
// raises it to the kernel max before the container starts.
const ethrexMaxOpenFiles = 32768

// ethrexAuxOffHeapBytes is an ESTIMATE (not a bound) of the off-heap RAM this
// writer commits beyond the two budgets above: the four shared batchSinks
// (account trie/flat + code/meta, ~2×64 MiB retained C buffer each), the
// per-worker storage sink pairs (Phase 0 and Stage B, 16 MiB threshold →
// ~2×32 MiB retained each; ≤1 GiB at the 16-worker default — see
// workerFlushThresholdBytes), the Pebble streamsort store's two 256 MiB
// memtable arenas, and RocksDB's compaction/iterator scratch.
const ethrexAuxOffHeapBytes = 2 * 1024 * 1024 * 1024

// ethrexAllocatorSlackBytes covers allocator memory no component reports:
// jemalloc's retained dirty pages and metadata over the C heap (RocksDB,
// Pebble, the WriteBatches).
//
// Measured, then sized — not guessed. On the 40 GB same-seed A/B that gated
// this series (96-core host, 73 GiB realized), the jemalloc leg's off-heap
// plateaued at ~4.4 GiB from ~35% of Phase 2 onward, against ~3 GiB of
// directly attributed usage — ~1.5–2 GiB of allocator overhead, FLAT once
// steady state was reached. The glibc+arena-cap baseline kept climbing to
// 6.2 GiB on the identical workload. This constant replaces the pre-jemalloc
// 12 GiB "fragmentation reserve", which was a guess standing in for exactly
// the retention jemalloc removes; memlimit's post-reserve margin absorbs
// this slack being an underestimate at larger scale.
const ethrexAllocatorSlackBytes = 1536 * 1024 * 1024 // 1.5 GiB

// ethrexOffHeapReserveBytes is the total RAM this writer commits OUTSIDE the
// Go heap. runImpl subtracts it from the host's memory ceiling before deriving
// GOMEMLIMIT, so the Go runtime budgets against what is actually left rather
// than against memory RocksDB, Pebble, and the C allocator have already taken.
const ethrexOffHeapReserveBytes = ethrexBlockCacheBytes +
	ethrexDBWriteBufferBytes +
	ethrexAuxOffHeapBytes +
	ethrexAllocatorSlackBytes

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
	cfIdxBadBlocks            = 20
)

// ethrexDB holds the open grocksdb handle and the 21 CF handles.
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
// Per-CF options that shape the final bytes (compression, block size, bloom
// filters, blob files, target_file_size_base) mirror the real ethrex client's
// crates/storage/backend/rocksdb.rs, so a state-actor-produced DB is
// byte-representative for benchmarking. Deliberate deviations are all
// process-runtime knobs that cannot change the compacted output: L0
// compaction triggers (ethrex 4/20/36, maxed here to avoid stalls during
// bulk import), state-CF memtables (256 MiB × 4 vs ethrex's 512 MiB × 6 —
// see the state-CF case), and the block cache (512 MiB vs ethrex's 12 GiB
// default —
// see ethrexBlockCacheBytes). Close() runs CompactRange afterward, rewriting
// every SST with the same compression/block/bloom options, so the final
// on-disk shape matches ethrex regardless.
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

	// The 21 named ethrex CFs, plus RocksDB's implicit "default" CF appended
	// LAST (index 21). RocksDB always creates "default" on a fresh DB, and an
	// open call must account for every existing CF or it errors with "you have
	// to open all column families". Appending it keeps the cfIdx* constants
	// (0..20) aligned with Tables; cfs[21] (default) is created but never
	// written. Mirrors besu's explicit CFDefault inclusion.
	cfNames := make([]string, 0, len(ethrexinternal.Tables)+1)
	cfNames = append(cfNames, ethrexinternal.Tables...)
	cfNames = append(cfNames, "default")

	// Shared LRU block cache across all CFs (see ethrexBlockCacheBytes).
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
		//
		// Do NOT "complete the pairing" with
		// SetPinL0FilterAndIndexBlocksInCache: with L0 uncompacted for the
		// whole import (~2800 files at 700 GB), pinning would push several
		// GiB past the cache's capacity — pinned usage may legally exceed an
		// LRU's size, defeating the bound this cache exists to provide.
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
			// 256 MiB × 4, matching the besu and nethermind writers — the two
			// grocksdb bulk paths proven at multi-hundred-GB scale. ethrex's
			// runtime figure is 512 MiB × 6 with min-merge 2, but memtable
			// shape is a process-runtime knob: it cannot change a byte of the
			// compacted output (Close() rewrites at target_file_size_base),
			// and mirroring it summed to a 12 GiB ceiling across these four
			// CFs. The per-CF sum now equals ethrexDBWriteBufferBytes, making
			// the global cap a true backstop instead of the operative flush
			// trigger. No SetMinWriteBufferNumberToMerge: no peer writer sets
			// it, and waiting on a second immutable memtable only raised
			// resident bytes and flush granularity. No memtable prefix bloom:
			// RocksDB allocates one only under a prefix extractor or
			// whole-key filtering (memtable.cc), neither configured here —
			// the previous SetMemTablePrefixBloomSizeRatio(0.2) was inert.
			opts.SetWriteBufferSize(256 << 20)
			opts.SetMaxWriteBufferNumber(4)
			opts.SetTargetFileSizeBase(256 << 20)
			// Without this, RocksDB's DEFAULT max_bytes_for_level_base
			// (256 MB) silently defeats the MaxInt32 L0 triggers above:
			// ComputeCompactionScore takes max(count-score, L0-size /
			// max_bytes_for_level_base), so L0 compacted CONTINUOUSLY
			// through the whole import — measured at 5-7× write
			// amplification (~1.4-2 TB physically written for a 273 GB
			// DB) before this was set. besu and nethermind both set
			// 2 GiB (client/besu/dbs_cgo.go, client/nethermind/
			// dbs_cgo.go); ethrex was the outlier. The value is
			// env-tunable for benchmarking the defer-everything-to-Close
			// variant — see ethrexStateCFLevelBaseBytes.
			opts.SetMaxBytesForLevelBase(ethrexStateCFLevelBaseBytes())
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
			// Also covers transaction_locations, whose dedicated arm in ethrex
			// carries these same values. ethrex additionally registers the
			// "tx_locations_merge" associative merge operator on that CF. It is
			// omitted here: a merge operator only affects merge writes and their
			// resolution, state-actor writes no transaction locations, and
			// RocksDB does not compare merge-operator names across opens — a CF
			// created without one reopens cleanly with one registered.
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
		// Serial, mirroring the besu and nethermind writers. The 12-goroutine
		// fan-out this replaces bought little — max_background_jobs
		// (bulkBackgroundJobs) already floored real parallelism at 8 — and
		// cost memory at the worst moment: with L0 never compacted during
		// the import, the L0→base merge takes EVERY L0 file of a CF as
		// input at ~2 MiB compaction readahead each (~2800 files across the
		// state CFs at 700 GB), so compacting the four state CFs
		// concurrently stacked ~4× that readahead on top of end-of-run
		// state. Serial caps the term at the largest single CF. State CFs
		// go first: they dominate, and an interrupt part-way leaves the
		// cheap CFs uncompacted rather than the expensive ones.
		//
		// The per-CF log lines are the only record of this phase: the 30s
		// sampler is stopped BEFORE Close (it reads DB properties, and
		// grocksdb does not guard against a closed handle), so without them
		// Close-time compaction memory would be invisible — the same
		// blindness that let #116's import-phase terms go unmeasured.
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
				start := time.Now()
				d.db.CompactRangeCF(d.cfs[idx], emptyRange)
				log.Printf("  ethrex: compacted %s in %s · mem %s · %s",
					ethrexinternal.Tables[idx],
					time.Since(start).Round(time.Second),
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

	// num-files-at-level<N> is a STRING property — reading it through the
	// int API silently fails and reported a constant 0 for entire runs
	// (while table-reader memory proved SSTs were accumulating). Parse the
	// string form instead.
	sumL0 := func() uint64 {
		var total uint64
		for _, idx := range stateCFsForMemoryReport {
			if idx >= len(d.cfs) || d.cfs[idx] == nil {
				continue
			}
			v := d.db.GetPropertyCF("rocksdb.num-files-at-level0", d.cfs[idx])
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				total += n
			}
		}

		return total
	}

	return fmt.Sprintf(
		"rocksdb memtables=%s table-readers=%s cache=%s cache-pinned=%s L0-files=%d",
		memstat.FormatBytes(sum("rocksdb.cur-size-all-mem-tables")),
		memstat.FormatBytes(sum("rocksdb.estimate-table-readers-mem")),
		memstat.FormatBytes(cacheUsage),
		memstat.FormatBytes(cachePinned),
		sumL0(),
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
