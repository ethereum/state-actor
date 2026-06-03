// Package ethrex implements state-actor's ethrex client writer.
//
// # On-disk layout
//
// ethrex uses a single RocksDB instance at <dbPath> (no sub-directory).
// The 20 column families (Tables in internal/ethrex/constants.go) are
// created at open time; all must be declared or RocksDB fails on subsequent
// reopens.
//
// Written CFs at genesis:
//   - account_trie_nodes: account MPT rows (one per node + one per leaf-full-path)
//   - storage_trie_nodes: per-account storage MPT rows (address-prefixed)
//   - account_codes: code hash → EncodeCode(bytecode)
//   - account_code_metadata: code hash → u64-BE(len(bytecode))
//   - chain_data: ChainConfig (key 0x80) + block-number sentinels (0x01, 0x04)
//   - headers: RLP(hash) → RLP(BlockHeader)
//   - bodies: RLP(hash) → 0xc3c0c0c0 (empty genesis body)
//   - block_numbers: RLP(hash) → u64-LE(0)
//   - canonical_block_hashes: u64-LE(0) → RLP(hash)  ← boot gate
//
// Empty (but declared) CFs:
//
//	account_flatkeyvalue, storage_flatkeyvalue, misc_values, and 8 others.
//
// Two sidecar files are written next to the DB:
//   - metadata.json: {"schema_version": 2} — required by ethrex Store::new.
//   - ethrex-genesis.json: full genesis JSON for `ethrex --network <path>`.
//
// # Boot path
//
// ethrex's add_initial_state short-circuits ("nothing to do") when
// canonical_block_hashes[0] → headers[hash] matches genesis.get_block().hash().
// state-actor writes a matching genesis header + canonical row, so ethrex
// never recomputes the state trie at boot.
//
// # Pinned release
//
// Tested against ethrex release v15.0.0 (lambdaclass/ethrex). The golden test
// in golden_test.go (cgo_ethrex-only) verifies byte-exactness against
// testdata/genesis_dump.json, which is byte-identical from v13.0.0 to v15.0.0.
//
// # Docker-only
//
// The cgo_ethrex build tag and librocksdb are only available inside the
// Dockerfile.ethrex build context. Local builds without the tag compile the
// stub (run_stub.go) which returns errNotImplemented.
//
// # In-memory Builder RAM ceiling
//
// internal/ethrex.Builder accumulates all trie leaves in memory before
// emitting node rows at Root(). For the e2e fixture (hundreds of thousands
// of accounts) and moderate --target-size this is correct and fast. For a
// multi-GB --target-size the full leaf set will not fit in RAM; a streaming
// Builder rewrite would be needed. The byte-exact golden tests guarantee any
// future streaming rewrite produces identical output.
//
// PreAlloc entities with large Storage iterators are also built in-memory
// (one in-memory Builder per entity). For spec PreAlloc with very large
// storage (millions of slots per entity), this consumes RAM proportional to
// the slot count. A future improvement would drain the storage iterator into
// a temporary streamsort before feeding the Builder.
package ethrex
