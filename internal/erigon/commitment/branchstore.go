//go:build cgo_erigon_commitment

package commitment

import (
	"fmt"
	"os"
	"strconv"

	"github.com/cockroachdb/pebble"
)

// branchStore is a LIVE, concurrency-safe read-write KV over a temp Pebble
// DB — the commitment branch sink. It replaces the in-memory
// mergedBranches map, which was Θ(total-keys) (tens of GB at bench scale
// because storage slots are hashed into the same unified trie). PutBranch →
// set, Branch → get, both live and safe for the 16 ConcurrentPatriciaHashed
// workers (Pebble Set/Get are goroutine-safe). Peak RAM is the Pebble
// memtable + cache, independent of branch count.
//
// Being read-write (not the write-once streamsort) is what enables
// incremental/chunked commitment: branches written by an earlier chunk's
// Process are visible to a later chunk's ctx.Branch reads. For a single
// from-empty genesis Process the read path is never hit for a written
// prefix (sorted single-pass folds each prefix once and never re-descends
// it), so get() returns nil there — byte-identical to the old map path.
type branchStore struct {
	dir   string
	db    *pebble.DB
	cache *pebble.Cache
}

// newBranchStore opens a fresh temp Pebble DB under tmpDir (empty →
// os.TempDir()). The block cache is the dominant speed lever for the
// CHUNKED/incremental path, where every chunk after the first re-reads
// earlier chunks' branches via ctx.Branch: a cache that holds the hot branch
// set keeps those reads in RAM. Sized by STATE_ACTOR_BRANCH_CACHE_GB
// (default 1 GiB). A 256 MiB memtable cuts L0 churn under the write load.
func newBranchStore(tmpDir string) (*branchStore, error) {
	dir, err := os.MkdirTemp(tmpDir, "commitment-branches-*")
	if err != nil {
		return nil, fmt.Errorf("commitment: mkdir branch store: %w", err)
	}
	cacheBytes := int64(1) << 30
	if v := os.Getenv("STATE_ACTOR_BRANCH_CACHE_GB"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			cacheBytes = int64(n) << 30
		}
	}
	cache := pebble.NewCache(cacheBytes)
	db, err := pebble.Open(dir, &pebble.Options{
		DisableWAL:   true,
		Cache:        cache,
		MemTableSize: 256 << 20,
	})
	if err != nil {
		cache.Unref()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("commitment: open branch store: %w", err)
	}
	return &branchStore{dir: dir, db: db, cache: cache}, nil
}

// set writes one branch (last-write-wins on a re-fold). Safe for the 16
// nibble-disjoint workers to call concurrently. The distinct-prefix count
// (== old len(mergedBranches)) is taken once at the end via iterate, so
// there's no per-write read amplification here.
func (b *branchStore) set(prefix, data []byte) error {
	if err := b.db.Set(prefix, data, pebble.NoSync); err != nil {
		return fmt.Errorf("commitment: branch store set: %w", err)
	}
	return nil
}

// get returns the branch at prefix, or (nil, nil) if absent. The returned
// slice is copied out of Pebble's buffer.
func (b *branchStore) get(prefix []byte) ([]byte, error) {
	v, closer, err := b.db.Get(prefix)
	if err == pebble.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("commitment: branch store get: %w", err)
	}
	out := make([]byte, len(v))
	copy(out, v)
	_ = closer.Close()
	return out, nil
}

// iterate yields every (prefix, data) in ascending key order. Called once
// after the walk to dump branches into the write-once commitment .kv
// streamsort. Key/value alias Pebble's buffers — copy if retained.
func (b *branchStore) iterate(yield func(prefix, data []byte) error) error {
	it, err := b.db.NewIter(nil)
	if err != nil {
		return fmt.Errorf("commitment: branch store iter: %w", err)
	}
	defer func() { _ = it.Close() }()
	for it.First(); it.Valid(); it.Next() {
		if err := yield(it.Key(), it.Value()); err != nil {
			return err
		}
	}
	return it.Error()
}

// close releases the DB + cache and removes the temp dir. Idempotent.
func (b *branchStore) close() {
	if b.db != nil {
		_ = b.db.Close()
		b.db = nil
	}
	if b.cache != nil {
		b.cache.Unref()
		b.cache = nil
	}
	if b.dir != "" {
		_ = os.RemoveAll(b.dir)
		b.dir = ""
	}
}
