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
// Nethermind configures every database and column family by concatenating
// RocksDB option strings — a global default plus per-DB/per-column
// overrides, later fragments winning — and parsing the result with
// rocksdb_get_options_from_string (DbOnTheRocks.BuildOptions). grocksdb
// exposes the same C entry point, so the strings below are executed
// verbatim and RocksDB's own parser guarantees identical semantics.
//
// Provenance: src/Nethermind/Nethermind.Db.Rocks/Config/DbConfig.cs at
// commit 2706ce9e02 (master, 2026-07-31). Byte-identical to the pinned
// 1.39.0 release except ReceiptsBlocksDb, which gained two keys on master
// — both overridden by applyBulkImportDeltas anyway.

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
// fragment. The mandatory "default" CF is absent from Nethermind's
// FlatDbColumns enum (RocksDbSharp seeds it with raw defaults there); we
// give it the FlatDb base instead — equivalent for a CF that stays empty.
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

// nethOptions composes Nethermind's effective option string — base +
// override fragments, later wins — parses it with RocksDB's parser, and
// layers the bulk-import tuning on top. Caller owns the returned Options.
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
// parsed Nethermind option bag. Runtime-only knobs — nothing here changes
// the SST format Nethermind later reads.
func applyBulkImportDeltas(opts *grocksdb.Options) {
	opts.SetCreateIfMissing(true)
	opts.SetCreateIfMissingColumnFamilies(true)
	opts.SetWriteBufferSize(perDBWriteBufferBytes)
	opts.SetMaxWriteBufferNumber(4)
	opts.SetLevel0FileNumCompactionTrigger(math.MaxInt32)
	opts.SetLevel0SlowdownWritesTrigger(math.MaxInt32)
	opts.SetLevel0StopWritesTrigger(math.MaxInt32)
	// Reversals of Nethermind runtime behaviours we have no equivalent for:
	// its persistence layer flushes the flat DB's WAL manually, and its
	// parallel commit path wants unordered_write; ours does neither.
	opts.SetManualWALFlush(false)
	opts.SetUnorderedWrite(false)
	parallelism := runtime.NumCPU()
	if parallelism > bulkBackgroundJobs {
		parallelism = bulkBackgroundJobs
	}
	opts.IncreaseParallelism(parallelism)
	opts.SetMaxBackgroundJobs(parallelism)
}
