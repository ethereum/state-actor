// Package ethrex implements state-actor's ethrex client writer.
//
// # On-disk layout
//
// A single RocksDB instance at <dbPath> with 20 column families (Tables in
// internal/ethrex/constants.go), all declared at open time. CFs written at genesis:
//   - account_trie_nodes / storage_trie_nodes: MPT structural + leaf-NODE-RLP rows
//     (storage rows are address-prefixed)
//   - account_flatkeyvalue / storage_flatkeyvalue: leaf full-path → value
//   - account_codes / account_code_metadata: code hash → EncodeCode / len
//   - chain_data: ChainConfig (key 0x80) + block-number sentinels (0x01, 0x04)
//   - misc_values: "last_written" → 0xff (FKV "fully generated" sentinel)
//   - headers / bodies / block_numbers / canonical_block_hashes: genesis block
//     (canonical_block_hashes[0] is the boot gate)
//
// Sidecars next to the DB: metadata.json ({"schema_version": 2}, required by
// ethrex Store::new) and ethrex-genesis.json (for `--network`).
//
// # Flat-KV (snap-synced-state) layer
//
// Like every other client, state-actor pre-populates the flat-KV CFs so the DB
// models a SNAP-synced node. Leaf values live ONLY in the flat-KV CFs, not
// duplicated as leaf full-path rows in the trie-node CFs — matching ethrex's own
// apply_trie_updates and snap-sync bulk builder. The trie-node CFs keep structural
// and leaf-NODE-RLP rows (carrying values for root/proofs). misc_values
// ["last_written"]=0xff makes ethrex's generator skip rebuilding on boot.
//
// # Boot path
//
// add_initial_state short-circuits when canonical_block_hashes[0] → headers[hash]
// matches genesis.get_block().hash(), so ethrex never recomputes state at boot.
//
// # Pinned releases
//
// Golden test: byte-exact vs testdata/genesis_dump.json, regenerated at ethrex
// v16.0.0; the state-bearing CFs are byte-identical v13–v16. E2e boot test
// (e2e_test.go) pins the same v16.0.0 image (first release with
// --skip-genesis-validation, lambdaclass/ethrex#6783).
//
// # Build
//
// cgo_ethrex + librocksdb are Docker-only (Dockerfile.ethrex); local builds
// without the tag compile the stub (run_stub.go → errNotImplemented).
//
// # Memory
//
// internal/ethrex.Builder is a streaming stack-trie — O(keyLen) RAM regardless of
// leaf count. PreAlloc storage streams through internal/streamingtrie in Phase 0;
// Phase 2 storage stays materialized and is bounded by construction.
package ethrex
