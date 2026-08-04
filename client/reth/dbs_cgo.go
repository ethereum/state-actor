//go:build cgo_reth

package reth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/linxGnu/grocksdb"

	iReth "github.com/ethereum/state-actor/internal/reth"
)

// Reth MDBX geometry — matches reth's default in
// crates/storage/db/src/implementation/mdbx/mod.rs.
//
// Page size: reth uses default_page_size() = OS page size clamped to
// [4096, 65536]. Passing 0 to mdbx-go is treated as MDBX_MIN_PAGESIZE
// (256 bytes), which would produce a database reth refuses to open
// on platforms where its expected page size differs (notably macOS
// arm64 with 16 KiB pages). We compute the same value reth would
// produce: clamp(os.Getpagesize(), 4096, 65536).
const (
	mdbxSizeMin      = int(0)
	mdbxSizeNow      = int(0)
	mdbxSizeMax      = int(8 * 1024 * 1024 * 1024 * 1024) // 8 TiB
	mdbxGrowthStep   = int(4 * 1024 * 1024 * 1024)        // 4 GiB
	mdbxShrinkThresh = int(0)

	// mdbxMaxReaders matches reth's DatabaseArguments default
	// (max_readers = 32_000); the value sizes the reader table in
	// mdbx.lck at env creation.
	mdbxMaxReaders = uint64(32_000)
)

// mdbxDefaultPageSize matches reth's default_page_size() in
// crates/storage/db/src/implementation/mdbx/mod.rs.
func mdbxDefaultPageSize() int {
	ps := os.Getpagesize()
	if ps < 4096 {
		return 4096
	}
	if ps > 65536 {
		return 65536
	}
	return ps
}

// rocksdbCFNames lists the v2 history-table column families reth uses.
var rocksdbCFNames = []string{
	"default",
	"AccountsHistory",
	"StoragesHistory",
	"TransactionHashNumbers",
}

// Reth RocksDB tuning — mirrors reth's v2 history DB configuration
// (commit 9cba8a1550): shared 128 MiB block cache with 16 KiB blocks,
// shared 4 GiB write-buffer manager with a 128 MiB write buffer per CF,
// LZ4 compression with Zstd at the bottommost level (compression
// disabled for TransactionHashNumbers), dynamic-level compaction,
// 6 background jobs, 512 open files, 1 MiB bytes-per-sync, and WAL
// TTL / size limit both 0.
const (
	rocksBlockCacheBytes   = 128 << 20 // 128 MiB shared block cache
	rocksBlockSizeBytes    = 16 << 10  // 16 KiB blocks
	rocksWriteBufferTotal  = 4 << 30   // 4 GiB shared write-buffer manager
	rocksWriteBufferPerCF  = 128 << 20 // 128 MiB write buffer per CF
	rocksMaxBackgroundJobs = 6
	rocksMaxOpenFiles      = 512
	rocksBytesPerSync      = 1 << 20 // 1 MiB
)

// Envs holds the open MDBX env + named DBIs and the RocksDB env + CFs.
// Caller must call Close() when done.
type Envs struct {
	Mdbx     *mdbx.Env
	MdbxDBIs map[string]mdbx.DBI

	RocksDB  *grocksdb.DB
	RocksCFs map[string]*grocksdb.ColumnFamilyHandle

	// historySink batches archive-mode AccountsHistory + StoragesHistory
	// writes into the RocksDB column families v2 reth reads from. Lazy-
	// initialised by HistorySink; Close drains + tears it down.
	historySink *historySink

	closed bool
}

// HistorySink returns the per-Envs archive-mode history sink, creating it
// on first call. NOT goroutine-safe — callers serialise puts (the streaming
// consumer is single-goroutine; the inline writers are called from one
// MDBX Update closure at a time).
func (e *Envs) HistorySink() *historySink {
	if e.historySink == nil {
		e.historySink = newHistorySink(e)
	}
	return e.historySink
}

// OpenEnvs creates a fresh datadir at dataDir and opens the MDBX env +
// RocksDB. freshDir=true REFUSES to open if any reth artifact (db/mdbx.dat,
// rocksdb/CURRENT, static_files/) is already present.
func OpenEnvs(dataDir string, freshDir bool) (*Envs, error) {
	if freshDir {
		if err := requireFreshDir(dataDir); err != nil {
			return nil, err
		}
	}

	dbDir := filepath.Join(dataDir, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dbDir, err)
	}
	rocksdbDir := filepath.Join(dataDir, "rocksdb")
	if err := os.MkdirAll(rocksdbDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", rocksdbDir, err)
	}

	envs := &Envs{
		MdbxDBIs: make(map[string]mdbx.DBI, len(iReth.Tables)),
		RocksCFs: make(map[string]*grocksdb.ColumnFamilyHandle, len(rocksdbCFNames)),
	}

	// --- MDBX env ---
	// mdbx-go v0.40+ requires a Label argument to NewEnv.
	env, err := mdbx.NewEnv(mdbx.Label("chaindata"))
	if err != nil {
		return nil, fmt.Errorf("mdbx.NewEnv: %w", err)
	}

	if err := env.SetGeometry(
		mdbxSizeMin,
		mdbxSizeNow,
		mdbxSizeMax,
		mdbxGrowthStep,
		mdbxShrinkThresh,
		mdbxDefaultPageSize(),
	); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.SetGeometry: %w", err)
	}

	if err := env.SetOption(mdbx.OptMaxDB, uint64(len(iReth.Tables))); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.SetOption(OptMaxDB): %w", err)
	}

	if err := env.SetOption(mdbx.OptMaxReaders, mdbxMaxReaders); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.SetOption(OptMaxReaders): %w", err)
	}

	// WriteMap + NoReadahead mirror reth's own MDBX env (mdbx/mod.rs:
	// writemap enabled, readahead disabled). SafeNoSync deviates from
	// reth's durable sync mode on purpose: it is a write-path-only flag
	// (not persisted in mdbx.dat) that buys bulk-write throughput, and
	// durability is owed at Envs.Close via the explicit Sync — the
	// artifact reth opens is byte-for-byte durable. NoMemInit skips
	// zero-fill on freshly-allocated pages (we overwrite them).
	// LifoReclaim improves cache locality for sequential writes.
	const envFlags = mdbx.WriteMap | mdbx.NoReadahead | mdbx.SafeNoSync | mdbx.NoMemInit | mdbx.LifoReclaim
	if err := env.Open(dbDir, envFlags, 0o644); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.Open(%s): %w", dbDir, err)
	}
	envs.Mdbx = env

	// Pre-resolve all named DBIs. Update (write txn) is required because
	// mdbx.Create needs write access.
	if err := env.Update(func(txn *mdbx.Txn) error {
		for _, ts := range iReth.Tables {
			flags := uint(mdbx.Create)
			if ts.DupSort {
				flags |= uint(mdbx.DupSort)
			}
			dbi, err := txn.OpenDBI(ts.Name, flags, nil, nil)
			if err != nil {
				return fmt.Errorf("OpenDBI(%s): %w", ts.Name, err)
			}
			envs.MdbxDBIs[ts.Name] = dbi
		}
		return nil
	}); err != nil {
		envs.Close()
		return nil, err
	}

	// --- RocksDB env with column families ---
	// All C-allocated option objects below are released via defer after
	// OpenDbColumnFamilies returns: the DB copies the options at open,
	// and the shared block cache / write-buffer manager are shared_ptr-
	// backed on the C++ side, so dropping our wrapper reference is safe
	// while the DB holds its own.
	blockCache := grocksdb.NewLRUCache(rocksBlockCacheBytes)
	defer blockCache.Destroy()
	writeBufferManager := grocksdb.NewWriteBufferManager(rocksWriteBufferTotal, false)
	defer writeBufferManager.Destroy()

	bbto := grocksdb.NewDefaultBlockBasedTableOptions()
	defer bbto.Destroy()
	bbto.SetBlockSize(rocksBlockSizeBytes)
	bbto.SetBlockCache(blockCache)

	rocksOpts := grocksdb.NewDefaultOptions()
	defer rocksOpts.Destroy()
	rocksOpts.SetCreateIfMissing(true)
	rocksOpts.SetCreateIfMissingColumnFamilies(true)
	rocksOpts.SetMaxBackgroundJobs(rocksMaxBackgroundJobs)
	rocksOpts.SetMaxOpenFiles(rocksMaxOpenFiles)
	rocksOpts.SetBytesPerSync(rocksBytesPerSync)
	rocksOpts.SetWriteBufferManager(writeBufferManager)
	rocksOpts.SetWALTtlSeconds(0)
	rocksOpts.SetWalSizeLimitMb(0)

	cfOpts := make([]*grocksdb.Options, len(rocksdbCFNames))
	for i, name := range rocksdbCFNames {
		o := grocksdb.NewDefaultOptions()
		defer o.Destroy()
		o.SetBlockBasedTableFactory(bbto)
		o.SetWriteBufferSize(rocksWriteBufferPerCF)
		o.SetLevelCompactionDynamicLevelBytes(true)
		if name == "TransactionHashNumbers" {
			o.SetCompression(grocksdb.NoCompression)
		} else {
			o.SetCompression(grocksdb.LZ4Compression)
			o.SetBottommostCompression(grocksdb.ZSTDCompression)
		}
		cfOpts[i] = o
	}

	rdb, cfs, err := grocksdb.OpenDbColumnFamilies(rocksOpts, rocksdbDir, rocksdbCFNames, cfOpts)
	if err != nil {
		envs.Close()
		return nil, fmt.Errorf("grocksdb.OpenDbColumnFamilies: %w", err)
	}
	envs.RocksDB = rdb
	for i, name := range rocksdbCFNames {
		envs.RocksCFs[name] = cfs[i]
	}

	return envs, nil
}

// requireFreshDir errors if dataDir already contains a reth datadir.
func requireFreshDir(dataDir string) error {
	for _, p := range []string{
		filepath.Join(dataDir, "db", "mdbx.dat"),
		filepath.Join(dataDir, "rocksdb", "CURRENT"),
		filepath.Join(dataDir, "static_files"),
	} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("requireFreshDir: %s already exists; refusing to overwrite", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("requireFreshDir stat %s: %w", p, err)
		}
	}
	return nil
}

// Close tears down the MDBX and RocksDB environments. Idempotent.
//
// On the MDBX side, an explicit Sync runs before Close so that
// MDBX_SAFE_NOSYNC deferred writes are flushed to disk on a clean
// process exit. force=true requests synchronous fdatasync;
// nonblock=false waits for completion.
//
// On the RocksDB side, FlushCFs forces the per-CF memtables to land as
// SST files before Close. Bulk writes go through historySink with
// WAL-disabled, so without this flush reth's first read could see an
// empty CF whose state lived only in the now-discarded memtable.
func (e *Envs) Close() error {
	if e == nil || e.closed {
		return nil
	}
	e.closed = true
	var firstErr error
	// Drain the history sink BEFORE FlushCFs — sink.Close moves any
	// pending batch into the RocksDB memtable; FlushCFs then drains the
	// memtable to SST files. Reversed order = silent data loss.
	if e.historySink != nil {
		if err := e.historySink.Close(); err != nil {
			firstErr = fmt.Errorf("historySink.Close: %w", err)
		}
		e.historySink = nil
	}
	if e.RocksDB != nil {
		flushOpts := grocksdb.NewDefaultFlushOptions()
		flushOpts.SetWait(true)
		cfs := make([]*grocksdb.ColumnFamilyHandle, 0, len(e.RocksCFs))
		for _, cf := range e.RocksCFs {
			cfs = append(cfs, cf)
		}
		if err := e.RocksDB.FlushCFs(cfs, flushOpts); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("rocksdb.FlushCFs: %w", err)
			}
		}
		flushOpts.Destroy()
		for _, cf := range e.RocksCFs {
			cf.Destroy()
		}
		e.RocksDB.Close()
	}
	if e.Mdbx != nil {
		if err := e.Mdbx.Sync(true, false); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mdbx.Sync: %w", err)
			}
		}
		e.Mdbx.Close()
	}
	return firstErr
}
