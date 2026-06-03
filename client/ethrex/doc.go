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
// # Streaming memory profile
//
// internal/ethrex.Builder is a streaming stack-trie: it holds only the rightmost
// spine (<= keyLen branch frames) plus the current leaf, so trie construction is
// O(keyLen) RAM regardless of leaf count. The byte-exact golden tests plus the
// randomized streaming-vs-recursive differential pin its output.
//
// PreAlloc entities with large Storage iterators stream too: Phase 0 drains each
// entity's Storage through internal/streamingtrie (disk-backed streamsort sort +
// keccak-ascending replay) into the Builder, so a single huge-storage contract
// (100M-1B slots) never materializes — mirroring reth's spec_storage_streaming.
// Phase 2 (GenesisStorage / AutoFill contract storage) is bounded by construction
// and stays materialized; if GenesisStorage ever carries huge maps it would need
// the same streaming treatment.
package ethrex
