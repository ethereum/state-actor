// Package nimbus implements state-actor's Nimbus (nimbus-eth1) client writer.
//
// # On-disk layout
//
// A single RocksDB instance at <dbPath>/ecdb (nimbus opens <data-dir>/ecdb
// unconditionally; there is no flag for it) with the four column families
// nimbus creates:
//   - AriVtx: every vertex of the account trie (root (1,1)) and of each
//     storage trie (root (stoID, stoID)), keyed by RootedVertexID, plus the
//     admin record under the EMPTY key (vTop ‖ serial=0 ‖ 0x7e).
//   - KvtGen: genesis metadata — 0x00‖hash → header RLP, 0x02‖hash → total
//     difficulty (0x80), 0x01‖LE64(0) → RLP(hash), 0x08‖LE64(0) → fcuHead,
//     0x04 0x00 → RLP(hash) (canonical head; nimbus's "already initialised"
//     gate, written LAST with sync) — and 0x06‖codeHash → raw bytecode for
//     every distinct non-empty code.
//   - KvtSync, default: created empty.
//
// Sidecar at the datadir root: nimbus-genesis.json (empty alloc) for
// `nimbus executionClient --network=<path>`.
//
// The vertex encoding, the static/dynamic vertex-ID scheme and the Kvt keys
// live in internal/nimbus; this package only decides WHEN each row is written
// and drives RocksDB.
//
// # Vertex IDs
//
// Storage-trie roots (stoIDs) come from one process-wide allocator
// (internal/nimbus.VidAllocator) in a deterministic order: Phase 0 allocates
// for spec entities in addrHash order before dispatching them, Stage A
// allocates for every other entity with a non-zero slot as the sorted stream
// is read. Branches deeper than STATIC_VID_LEVELS take 16-ID blocks from the
// same allocator on whichever goroutine builds them, so their numbering
// varies run to run; IDs are never hashed and never overlap the static range,
// so the state root and nimbus's reads are unaffected. The admin record's
// vTop is the allocator's high-water mark and is written before the genesis
// rows — without it nimbus would restart the counter at FIRST_DYNAMIC_VID and
// collide with the written stoIDs.
//
// Every branch record carries its 32-byte Merkle key (when the node RLP is
// >= 32 bytes, always true for a root). Nimbus trusts stored keys, and its
// boot-time computeStateRoot(skipLayers=true) returns as soon as (1,1) has
// one; a root without a key triggers a serial walk of the whole trie
// ("Writing computeKey cache").
//
// # Boot path
//
// Nimbus rebuilds genesis from --network's alloc on every start and refuses a
// datadir whose block-0 hash differs (preventLoadingDataDirForTheWrongNetwork).
// The sidecar's alloc is empty, so boot with --debug-rewrite-datadir-id, the
// hidden flag that skips that check — the analogue of ethrex's
// --skip-genesis-validation. With header 0 present CommonRef.init never
// rewrites genesis, initializeDb sees the canonical-head row and skips, and
// ForkedChain loads base = SavedState.serial (0) through 0x01‖LE64(0) → header.
// Full recipe: docs/RUNBOOK.md#nimbus; validated by e2e_test.go.
//
// # Pin
//
// statusim/nimbus-eth1:master-2f0ae87 (commit 2f0ae87cd, "Storage trie static
// vids (#4797)"). A master build: that commit changed the on-disk format and
// no release reads it. internal/nimbus/doc.go and
// internal/nimbus/testdata/gen/README.md carry the same pin; move them
// together. Golden tests: TestNimbusGoldenStateRoot (canonical root +
// internal/nimbus.VerifyState over the written rows) and TestGenesisDumpGolden
// (byte-exact vs a dump produced by nimbus's own genesis path).
//
// # Build
//
// cgo_nimbus + librocksdb are Docker-only (Dockerfile.nimbus); local builds
// without the tag compile the stub (run_stub.go → errNotImplemented).
//
// # Memory
//
// Same shape as the ethrex writer: RocksDB's block cache, memtables, the
// WriteBatches and allocator slack are budgeted by the constants in
// dbs_cgo.go (nimbusOffHeapReserveBytes) and subtracted from the host ceiling
// before internal/memlimit caps the Go heap. Vertex records are at most 118
// bytes and keys at most 17, so per-row overhead dominates; nothing here
// changes the logical database — Close()'s KForce CompactRange rewrites every
// SST with the per-CF options either way.
package nimbus
