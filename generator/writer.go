package generator

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

// WriterStats holds cumulative byte counts from state writes.
type WriterStats struct {
	AccountBytes uint64
	StorageBytes uint64
	CodeBytes    uint64
}

// Writer is the abstraction for client-specific state writers (geth, reth,
// nethermind). It captures the surface that Generator drives during state
// generation: per-entity writes, RLP-fast paths, lifecycle hooks, and stats.
//
// Implementations live in client/<name>/ packages. The geth implementation
// (client/geth.Writer) writes to a Pebble DB using snapshot-layer keys; future
// implementations may target different on-disk formats.
//
// All methods must be safe to call from the Generator's pipeline goroutines
// (typically a single producer plus parallel batch workers behind the scenes).
type Writer interface {
	// WriteAccount writes one account. addrHash is pre-computed keccak256(addr)
	// so the caller can amortize hashing across snapshot/trie writes. The
	// incarnation parameter is reserved for Erigon-style schemas; geth ignores it.
	WriteAccount(addr common.Address, addrHash common.Hash, acc *types.StateAccount, incarnation uint64) error

	// WriteStorage writes one storage slot. addrHash and slotHash are pre-computed
	// keccak256 hashes; raw addr/slot are passed through for backends that need them.
	WriteStorage(addr common.Address, addrHash common.Hash, slot common.Hash, slotHash common.Hash, value common.Hash) error

	// WriteStorageRLP writes a storage slot whose value is already RLP-encoded.
	// Avoids double-encoding when the caller has the bytes ready.
	WriteStorageRLP(addrHash common.Hash, slotHash common.Hash, valueRLP []byte) error

	// WriteRawStorage writes a storage slot with a pre-hashed trie key. The
	// hashedSlot bypasses keccak256 and is used directly as the storage key.
	WriteRawStorage(addr common.Address, incarnation uint64, hashedSlot, value common.Hash) error

	// WriteCode writes contract bytecode keyed by its keccak256 hash.
	WriteCode(codeHash common.Hash, code []byte) error

	// SetStateRoot records the post-generation state root and any backend-specific
	// metadata (geth: PathDB state ID, snapshot generator marker; nethermind: n/a).
	// binaryTrie selects the on-disk namespace for clients whose pathdb wraps
	// its diskdb under a prefix in bintrie mode (geth uses "v"); backends that
	// do not care may ignore the flag.
	SetStateRoot(root common.Hash, binaryTrie bool) error

	// Flush commits all pending writes and tears down the async pipeline.
	// Shutdown-once: do not call mid-run.
	Flush() error

	// FlushBatch synchronously flushes the buffered batch and drains in-flight
	// async writes without tearing down the pipeline. Safe to call mid-generation
	// (e.g. before a dirSize sample) provided no concurrent Write* calls are in flight.
	FlushBatch() error

	// Close releases backend resources (DB handle, async workers).
	Close() error

	// Stats reports cumulative byte counts.
	Stats() WriterStats

	// DB exposes the underlying go-ethereum key-value store for backends that
	// support it (geth Pebble). Returns nil for backends that do not (e.g.
	// nethermind, which writes to grocksdb directly). Callers that depend on a
	// non-nil result must check before use.
	DB() ethdb.KeyValueStore
}

// WriterFactory builds a Writer from a Config. Each client/<name>/ package
// provides one and registers it via RegisterDefaultWriterFactory in init().
type WriterFactory func(cfg Config) (Writer, error)

// defaultWriterFactory is the factory used by New(). Set via
// RegisterDefaultWriterFactory, typically from a client package's init().
var defaultWriterFactory WriterFactory

// RegisterDefaultWriterFactory installs f as the writer factory for New().
// Calling twice replaces the previous factory; the last import wins.
//
// Production code in package generator must NOT import any client/<name>/
// package (that would create a generator → client/<name> → generator cycle).
// Instead, callers (main.go, e2e tests) import a client package by name to
// trigger its init() which calls this function.
func RegisterDefaultWriterFactory(f WriterFactory) {
	defaultWriterFactory = f
}

// resolveDefaultWriterFactory returns the registered factory or a clear error
// if none is set. Used by New() to surface missing imports loudly rather than
// returning a nil-writer Generator that crashes later.
func resolveDefaultWriterFactory() (WriterFactory, error) {
	if defaultWriterFactory == nil {
		return nil, fmt.Errorf("no default writer factory registered: import a client package (e.g. _ \"github.com/nerolation/state-actor/client/geth\") or use NewWithWriter")
	}
	return defaultWriterFactory, nil
}

// MPTGeneratorFunc drives MPT-mode state generation end-to-end: opens its
// own backing DB, runs the two-phase entitygen → temp Pebble → production
// pipeline, writes metadata, and returns the result Stats. A client/<name>/
// package supplies an implementation by calling RegisterDefaultMPTGenerator
// in init(); generator.Generator.Generate() routes MPT mode to it.
//
// This decouples MPT-specific writer logic (which knows about snapshot
// keys, trie node layouts, PathDB metadata) from the generator package,
// while keeping the public surface — generator.New(cfg).Generate() — the
// same.
type MPTGeneratorFunc func(cfg Config) (*Stats, error)

// defaultMPTGenerator is the MPT pipeline used by Generator.Generate when
// TrieMode == TrieModeMPT. Set via RegisterDefaultMPTGenerator, typically
// from a client package's init().
var defaultMPTGenerator MPTGeneratorFunc

// RegisterDefaultMPTGenerator installs f as the MPT pipeline. Calling
// twice replaces the previous registration; the last import wins.
//
// Production code in package generator must NOT import client/<name>/
// directly; the registration pattern is how client packages contribute
// their MPT pipeline without creating an import cycle.
func RegisterDefaultMPTGenerator(f MPTGeneratorFunc) {
	defaultMPTGenerator = f
}

// resolveDefaultMPTGenerator returns the registered MPT pipeline or a
// clear error pointing at the missing client import.
func resolveDefaultMPTGenerator() (MPTGeneratorFunc, error) {
	if defaultMPTGenerator == nil {
		return nil, fmt.Errorf("no default MPT generator registered: import a client package (e.g. _ \"github.com/nerolation/state-actor/client/geth\")")
	}
	return defaultMPTGenerator, nil
}
