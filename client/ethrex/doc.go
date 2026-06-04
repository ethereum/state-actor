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
//   - account_trie_nodes: account MPT structural + leaf-NODE-RLP rows
//   - storage_trie_nodes: per-account storage MPT structural + leaf-NODE rows (address-prefixed)
//   - account_flatkeyvalue: account leaf full-path → account RLP (synced-state layer)
//   - storage_flatkeyvalue: address-prefixed storage leaf full-path → storage value
//   - account_codes: code hash → EncodeCode(bytecode)
//   - account_code_metadata: code hash → u64-BE(len(bytecode))
//   - chain_data: ChainConfig (key 0x80) + block-number sentinels (0x01, 0x04)
//   - misc_values: "last_written" → 0xff (FKV "fully generated" sentinel)
//   - headers: RLP(hash) → RLP(BlockHeader)
//   - bodies: RLP(hash) → 0xc3c0c0c0 (empty genesis body)
//   - block_numbers: RLP(hash) → u64-LE(0)
//   - canonical_block_hashes: u64-LE(0) → RLP(hash)  ← boot gate
//
// Empty (but declared) CFs: 8 others (pending_blocks, snap_state, etc.).
//
// # Flat-KV (snap-synced-state) layer
//
// ethrex serves state reads from the flat-KV CFs once a background generator has
// swept the trie post-sync; real ethrex genesis leaves them empty. state-actor
// pre-populates them so the produced DB models a SYNCED node — matching every
// other client, which all fake a synced flat layer (geth snapshot, reth
// HashedAccounts/Storages, besu Bonsai flat).
//
// We model a SNAP-synced node specifically: leaf values live ONLY in the flat-KV
// CFs, not duplicated as leaf full-path rows in the trie-node CFs. This matches
// how ethrex itself persists state — apply_trie_updates routes leaf rows
// (len 65/131) to the flat-KV CF, and the snap-sync bulk builder (trie_from_sorted)
// writes no leaf full-path rows. The trie-node CFs keep the structural and
// leaf-NODE-RLP rows, which carry the value for root/proof computation, so the
// trie is complete; the flat-KV key/value are byte-identical to what a leaf
// full-path row would be (ethrex apply_prefix == our PrefixedSink, flat value ==
// leaf value). A genesis-booted node would additionally carry those duplicate
// leaf rows in the trie-node CFs; a snap-synced node does not, so this layout is
// both representative and smaller. misc_values["last_written"]=0xff is stamped so
// ethrex's generator short-circuits on boot rather than rebuilding the layer.
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
