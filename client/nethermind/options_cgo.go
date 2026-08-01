//go:build cgo_neth

package nethermind

import (
	"fmt"
	"math"
	"runtime"
	"strings"

	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/neth/flat"
)

// This file makes state-actor's Nethermind databases carry Nethermind's own
// RocksDB configuration instead of grocksdb defaults.
//
// Nethermind configures every database (and every column family) by
// concatenating RocksDB option STRINGS — a global default plus per-DB
// overrides — and feeding the result through rocksdb_get_options_from_string
// (DbOnTheRocks.BuildOptions). grocksdb exposes the same C entry point as
// GetOptionsFromString, so instead of hand-porting each knob we execute
// Nethermind's exact strings, copied verbatim below, and RocksDB's own
// parser guarantees identical semantics.
//
// String provenance: src/Nethermind/Nethermind.Db.Rocks/Config/DbConfig.cs
// at commit 2706ce9e024a679c38c18a9329c7f9f4ba7282da (2026-07-31, tracking
// 1.39-dev). Composition rule (PerTableDbConfig.ReadRocksdbOptions):
// effective = RocksDbOptions + <Table>DbRocksDbOptions for plain DBs, and
// RocksDbOptions + <Table>DbRocksDbOptions + <Table><Column>DbRocksDbOptions
// for column DBs; later fragments win. Like Nethermind's
// NormalizeRocksDbOptions, duplicated optimize_filters_for_hits entries are
// collapsed to the last occurrence before parsing — RocksDB rejects that
// specific key appearing twice in one string.
//
// The strings decide everything persisted in the SST files Nethermind will
// later read: filter policies (bloomfilter:15 on state, ribbonfilter:10:3 on
// the flat CFs, none on code), per-DB optimize_filters_for_hits (whether the
// bottommost level keeps its filters), index types, block sizes, compression,
// format_version, target file sizes. Transient bulk-import tuning is layered
// on top afterwards via typed setters (applyBulkImportDeltas) and leaves no
// trace in the generated files.

// nethDefaultOptions is DbConfig.RocksDbOptions — the base every DB inherits.
const nethDefaultOptions = "min_write_buffer_number_to_merge=1;" +
	"write_buffer_size=16000000;" +
	"max_write_buffer_number=2;" +
	"memtable_whole_key_filtering=true;" +
	"memtable_prefix_bloom_size_ratio=0.02;" +
	"level_compaction_dynamic_level_bytes=false;" +
	"max_compaction_bytes=4000000000;" +
	"compression=kSnappyCompression;" +
	"optimize_filters_for_hits=true;" +
	"advise_random_on_open=true;" +
	"target_file_size_base=64000000;" +
	"max_bytes_for_level_base=256000000;" +
	"block_based_table_factory.block_size=16000;" +
	"block_based_table_factory.pin_l0_filter_and_index_blocks_in_cache=true;" +
	"block_based_table_factory.cache_index_and_filter_blocks_with_high_priority=true;" +
	"block_based_table_factory.format_version=5;" +
	"block_based_table_factory.index_type=kTwoLevelIndexSearch;" +
	"block_based_table_factory.partition_filters=true;" +
	"block_based_table_factory.metadata_block_size=4096;" +
	"block_based_table_factory.filter_policy=bloomfilter:10;"

// Per-DB overrides, DbConfig.<Name>DbRocksDbOptions.
const (
	nethStateOptions = "compression=kLZ4Compression;" +
		"max_bytes_for_level_multiplier=30;" +
		"max_bytes_for_level_base=350000000;" +
		"target_file_size_multiplier=2;" +
		"min_write_buffer_number_to_merge=2;" +
		"block_based_table_factory.block_restart_interval=4;" +
		"block_based_table_factory.data_block_index_type=kDataBlockBinaryAndHash;" +
		"block_based_table_factory.data_block_hash_table_util_ratio=0.5;" +
		"block_based_table_factory.block_size=32000;" +
		"block_based_table_factory.filter_policy=bloomfilter:15;" +
		"unordered_write=true;" +
		"max_write_batch_group_size_bytes=4000000;" +
		"ttl=0;" +
		"periodic_compaction_seconds=0;"

	nethCodeOptions = "write_buffer_size=16000000;" +
		"block_based_table_factory.block_cache=16000000;" +
		"optimize_filters_for_hits=false;" +
		"prefix_extractor=capped:8;" +
		"block_based_table_factory.index_type=kHashSearch;" +
		"block_based_table_factory.block_size=4096;" +
		"memtable=prefix_hash:1000000;" +
		"block_based_table_factory.filter_policy=null;" +
		"allow_concurrent_memtable_write=false;"

	nethBlocksOptions = "write_buffer_size=64000000;" +
		"block_based_table_factory.block_cache=32000000;" +
		"compaction_pri=kOldestLargestSeqFirst;" +
		"optimize_filters_for_hits=false;"

	nethHeadersOptions = "write_buffer_size=8000000;" +
		"block_based_table_factory.block_cache=32000000;" +
		"compaction_pri=kOldestLargestSeqFirst;" +
		"optimize_filters_for_hits=false;" +
		"block_based_table_factory.block_size=32000;" +
		"max_bytes_for_level_base=128000000;"

	nethBlockNumbersOptions = "write_buffer_size=8000000;" +
		"max_bytes_for_level_base=16000000;" +
		"block_based_table_factory.block_cache=16000000;" +
		"block_based_table_factory.block_size=4096;" +
		"optimize_filters_for_hits=false;" +
		"memtable=prefix_hash:1000000;" +
		"allow_concurrent_memtable_write=false;"

	nethBlockInfosOptions = "write_buffer_size=4000000;" +
		"max_bytes_for_level_base=32000000;" +
		"optimize_filters_for_hits=false;" +
		"block_based_table_factory.block_cache=16000000;" +
		"block_based_table_factory.block_size=32000;" +
		"compaction_pri=kOldestLargestSeqFirst;"

	nethReceiptsOptions = "write_buffer_size=2000000;" +
		"block_based_table_factory.block_cache=8000000;" +
		"optimize_filters_for_hits=false;"

	// Receipts column overrides (ReceiptsDefaultDb / ReceiptsTransactionsDb
	// are empty in DbConfig; only the Blocks column overrides).
	nethReceiptsBlocksCFOptions = "compaction_pri=kOldestLargestSeqFirst;" +
		"write_buffer_size=16000000;" +
		"max_write_buffer_number=4;"

	// FlatDbRocksDbOptions — common base of every flat-state column.
	nethFlatOptions = "min_write_buffer_number_to_merge=2;" +
		"block_based_table_factory.block_restart_interval=4;" +
		"block_based_table_factory.data_block_index_type=kDataBlockBinaryAndHash;" +
		"block_based_table_factory.data_block_hash_table_util_ratio=0.7;" +
		"block_based_table_factory.block_size=16000;" +
		"block_based_table_factory.filter_policy=ribbonfilter:10:3;" +
		"max_write_batch_group_size_bytes=4000000;" +
		"block_based_table_factory.pin_l0_filter_and_index_blocks_in_cache=true;" +
		"block_based_table_factory.prepopulate_block_cache=kFlushOnly;" +
		"block_based_table_factory.whole_key_filtering=true;" +
		"level_compaction_dynamic_level_bytes=false;" +
		"block_based_table_factory.partition_filters=false;" +
		"block_based_table_factory.index_type=kBinarySearch;" +
		"ttl=0;" +
		"periodic_compaction_seconds=0;" +
		"compression=kLZ4Compression;" +
		"target_file_size_multiplier=2;" +
		"manual_wal_flush=true;" +
		"uncache_aggressiveness=1000;" +
		"write_buffer_size=1000000;"

	nethFlatMetadataCFOptions = "max_bytes_for_level_base=1000000;"

	nethFlatAccountCFOptions = "compression=kNoCompression;" +
		"optimize_filters_for_hits=false;" +
		"target_file_size_multiplier=3;" +
		"target_file_size_base=32000000;" +
		"max_bytes_for_level_multiplier=15;" +
		"max_bytes_for_level_base=128000000;" +
		"block_based_table_factory.block_size=4096;" +
		"write_buffer_size=16000000;" +
		"max_write_buffer_number=4;"

	nethFlatStorageCFOptions = "optimize_filters_for_hits=false;" +
		"target_file_size_base=64000000;" +
		"block_based_table_factory.block_size=8000;" +
		"write_buffer_size=32000000;" +
		"max_write_buffer_number=4;"

	// DbConfig.FlatDbCommonTrieOptions, shared by the trie-node columns.
	nethFlatCommonTrieOptions = "level_compaction_dynamic_level_bytes=true;" +
		"block_based_table_factory.block_restart_interval=8;" +
		"block_based_table_factory.block_size=16000;"

	nethFlatStateTopNodesCFOptions = nethFlatCommonTrieOptions +
		"write_buffer_size=64000000;" +
		"max_write_buffer_number=4;"

	nethFlatStateNodesCFOptions = nethFlatCommonTrieOptions +
		"write_buffer_size=32000000;" +
		"max_write_buffer_number=4;"

	nethFlatStorageNodesCFOptions = nethFlatCommonTrieOptions +
		"max_bytes_for_level_base=350000000;" +
		"write_buffer_size=64000000;" +
		"max_write_buffer_number=8;"

	nethFlatFallbackNodesCFOptions = nethFlatCommonTrieOptions +
		"max_bytes_for_level_base=4000000;"
)

// nethPerDBOptions maps the single-CF database names to their DbConfig
// override fragment.
var nethPerDBOptions = map[string]string{
	dbNameState:        nethStateOptions,
	dbNameCode:         nethCodeOptions,
	dbNameBlocks:       nethBlocksOptions,
	dbNameHeaders:      nethHeadersOptions,
	dbNameBlockNumbers: nethBlockNumbersOptions,
	dbNameBlockInfos:   nethBlockInfosOptions,
}

// nethFlatCFOptions maps flat.Column ordinals to their column override
// fragment. The "default" CF has no Flat<Column>Db entry in DbConfig and
// inherits the FlatDb base unchanged.
var nethFlatCFOptions = map[flat.Column]string{
	flat.ColMetadata:      nethFlatMetadataCFOptions,
	flat.ColAccount:       nethFlatAccountCFOptions,
	flat.ColStorage:       nethFlatStorageCFOptions,
	flat.ColStateNodes:    nethFlatStateNodesCFOptions,
	flat.ColStateTopNodes: nethFlatStateTopNodesCFOptions,
	flat.ColStorageNodes:  nethFlatStorageNodesCFOptions,
	flat.ColFallbackNodes: nethFlatFallbackNodesCFOptions,
}

// normalizeOptimizeFiltersForHits ports Nethermind's NormalizeRocksDbOptions:
// keep only the LAST optimize_filters_for_hits= entry, because RocksDB's
// options-string parser does not allow the key to appear twice.
func normalizeOptimizeFiltersForHits(s string) string {
	const key = "optimize_filters_for_hits="
	last := strings.LastIndex(s, key)
	if last == -1 {
		return s
	}
	for {
		i := strings.Index(s, key)
		if i == -1 || i == last {
			return s
		}
		end := strings.IndexByte(s[i:], ';')
		if end == -1 {
			return s
		}
		s = s[:i] + s[i+end+1:]
		last = strings.LastIndex(s, key)
	}
}

// nethOptions composes Nethermind's effective option string for one DB (or
// one column family) — base + override fragments, later wins — parses it
// with RocksDB's own parser, and layers state-actor's transient bulk-import
// tuning on top. The caller owns the returned Options (Destroy after close).
func nethOptions(fragments ...string) (*grocksdb.Options, error) {
	combined := normalizeOptimizeFiltersForHits(nethDefaultOptions + strings.Join(fragments, ""))

	base := grocksdb.NewDefaultOptions()
	defer base.Destroy()
	opts, err := grocksdb.GetOptionsFromString(base, combined)
	if err != nil {
		return nil, fmt.Errorf("parse Nethermind option string: %w", err)
	}
	applyBulkImportDeltas(opts)
	return opts, nil
}

// applyBulkImportDeltas overlays state-actor's one-shot import tuning on a
// parsed Nethermind option bag. Everything here is runtime-only — memtable
// sizing, compaction scheduling, WAL flushing — and changes nothing about
// the SST format Nethermind later reads. The write path stays the same as
// the previous bulkRocksOptions: no compactions during the import, one
// CompactRange at Close.
func applyBulkImportDeltas(opts *grocksdb.Options) {
	opts.SetCreateIfMissing(true)
	opts.SetCreateIfMissingColumnFamilies(true)
	opts.SetWriteBufferSize(perDBWriteBufferBytes)
	opts.SetMaxWriteBufferNumber(4)
	opts.SetLevel0FileNumCompactionTrigger(math.MaxInt32)
	opts.SetLevel0SlowdownWritesTrigger(math.MaxInt32)
	opts.SetLevel0StopWritesTrigger(math.MaxInt32)
	// Nethermind flushes the flat DB's WAL manually from its persistence
	// layer; state-actor has no equivalent, so let RocksDB manage the WAL.
	opts.SetManualWALFlush(false)
	// Nethermind's state DB enables unordered_write for its parallel commit
	// path; our writer relies on ordered batch semantics.
	opts.SetUnorderedWrite(false)
	parallelism := runtime.NumCPU()
	if parallelism > bulkBackgroundJobs {
		parallelism = bulkBackgroundJobs
	}
	opts.IncreaseParallelism(parallelism)
	opts.SetMaxBackgroundJobs(parallelism)
}
