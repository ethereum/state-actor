// Package reth produces a fully-bootable Reth datadir directly from Go,
// without spawning the `reth` binary.
//
// # How it works
//
// Build the package with `-tags cgo_reth` (Docker-only — see
// Dockerfile.reth) and call RunCgo:
//
//	stats, err := reth.RunCgo(ctx, cfg, reth.Options{})
//
// RunCgo writes the on-disk artifacts reth boot validates. The datadir is a
// v2 layout (Metadata.storage_settings = {"storage_v2":true}):
//
//   - <datadir>/db/mdbx.dat — MDBX env. Canonical state lives in
//     HashedAccounts + HashedStorages; Plain* tables are declared but
//     empty under storage_v2. Trie + changeset + metadata tables
//     (AccountsTrie, StoragesTrie, AccountChangeSets, StorageChangeSets,
//     Bytecodes, StageCheckpoints, HeaderNumbers, BlockBodyIndices,
//     PruneCheckpoints, etc.) stay MDBX-resident.
//   - <datadir>/db/database.version — schema version sentinel ("2")
//   - <datadir>/rocksdb/* — RocksDB env with the v2 history CFs
//     (AccountsHistory + StoragesHistory + TransactionHashNumbers + the
//     default CF). The two history CFs receive writes under
//     cfg.Archive=true; default-mode runs leave them empty (PruneCheckpoint
//     markers tell reth's read path "history pruned before block 1",
//     routing historical-tag queries through HashedAccounts).
//   - <datadir>/static_files/static_file_<segment>_0_499999 (no extension)
//     + .conf + .off (+ .csoff for change-based segments) — block-0
//     nippy-jar segment files for headers, transactions, receipts,
//     transaction-senders, account-change-sets, storage-change-sets. The
//     two change-based segments are required as empty bootstrap shells so
//     reth's persistence service can append block 1 cleanly.
//   - <datadir>/chainspec.json — sidecar reth boot revalidates
//
// The state root in the genesis header is computed from the generated
// entities via the streaming HashBuilder in internal/reth, matching what
// trie.NewStackTrie produces and what reth itself would compute on a fresh
// init.
//
// # Streaming Phase 4
//
// Phase 4 generates entities in 100K batches.
// Each batch flows through WriteEOAs/WriteContracts to MDBX; the
// per-account RLP is then keyed by AddrHash and written into a
// Pebble-backed temp sorter (mirrors client/nethermind/entitygen_cgo.go).
// After all batches the sorter is iterated in addrHash-sorted order and
// each leaf is fed into the HashBuilder for the global state root.
//
// Peak Phase 4 RAM is bounded by one batch (~20 MiB at 100K accounts ×
// ~200 B per *Account) plus Pebble's 64 MiB write buffer plus
// max-slots-per-contract for storage tries — independent of total N.
//
// Phase 5a (chainspec.json) is now a fixed ~1 KB regardless of N: the
// chainspec carries only the chain config (chainID + hardfork timestamps)
// and the header bits reth needs (gasLimit, baseFeePerGas, difficulty,
// etc.). The `alloc` field is intentionally an empty object — state-actor
// direct-writes the genesis state into MDBX, and reth boots with
// `--debug.skip-genesis-validation` so it trusts the DB-resident state
// instead of recomputing the genesis hash from chainspec.alloc.
//
// The `--debug.skip-genesis-validation` flag is an upstream paradigmxyz/reth
// addition (digest-pinned via internal/reth/constants.go to a nightly
// snapshot containing #23919). Without that flag, reth's
// init_genesis_with_settings rejects the boot with GenesisHashMismatch
// because the alloc-derived genesis hash (empty MPT root) differs from
// the DB-resident genesis hash.
//
// # Build tag gating
//
// The cgo path lives behind `//go:build cgo_reth`. Without that tag,
// RunCgo returns runCgoNotAvailableError pointing at Dockerfile.reth
// (see run_stub.go). Local Go builds without libmdbx + librocksdb
// headers remain compilable but cannot exercise the cgo path.
//
// # Validation
//
// The boot oracle in oracle_test.go (//go:build cgo_reth oracle) drives
// `paradigmxyz/reth db stats` and `reth node --dev` against
// state-actor-generated datadirs and verifies via JSON-RPC that
// eth_getBalance / eth_getCode / eth_getStorageAt return the expected
// values. Run via `make test-reth-boot`.
//
// # Source layout
//
//   - run_cgo.go / run_stub.go: build-tag-gated RunCgo entry point
//   - dbs_cgo.go: MDBX env + RocksDB column families
//   - data_writer_cgo.go: per-EOA state-table writes
//   - bytecode_writer_cgo.go: deduped bytecode writes
//   - storage_writer_cgo.go: per-slot storage-table writes
//   - contracts_writer_cgo.go: composed contract writes
//   - metadata_cgo.go: minimum-boot MDBX metadata
//   - static_files_cgo.go: nippy-jar block-0 segment files
//   - sidecars.go: database.version writer
//   - state_root.go / storage_root.go: HashBuilder-driven state-root
//     computation (sliced + streaming variants)
//   - internal/streamsort: Pebble-backed temp sorter for streaming Phase 4
//   - chainspec.go: chainspec JSON writer (built from cfg.Genesis)
//   - header.go: genesis header construction
//   - options.go: Options struct (reserved); buildAllocAccounts helper
package reth
