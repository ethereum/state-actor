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
// RunCgo writes the on-disk artifacts reth boot validates:
//
//   - <datadir>/db/mdbx.dat — MDBX env with all named DBIs
//     (PlainAccountState, HashedAccounts, AccountChangeSets,
//     AccountsHistory, PlainStorageState, HashedStorages,
//     StorageChangeSets, StoragesHistory, Bytecodes, plus 15 metadata
//     tables incl. StageCheckpoints/Metadata/HeaderNumbers/etc.)
//   - <datadir>/db/database.version — schema version sentinel ("2")
//   - <datadir>/rocksdb/* — RocksDB env with v2 history-table column
//     families
//   - <datadir>/static_files/{headers,transactions,receipts,
//     transaction-senders}/static_file_*_0_499999.{conf,sf,off} — block-0
//     segment files in the nippy-jar format
//   - <datadir>/chainspec.json — sidecar reth boot revalidates
//
// The state root in the genesis header is computed from the generated
// entities via the streaming HashBuilder in internal/reth, matching what
// trie.NewStackTrie produces and what reth itself would compute on a fresh
// init.
//
// # Streaming Phase 4
//
// Phase 4 generates entities in batches of cfg.BatchSize (default 100K).
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
// addition (currently SHA-pinned via internal/reth/constants.go to a
// CPerezz/reth fork branch with the patch). Without that flag, reth's
// init_genesis_with_settings will reject the boot with GenesisHashMismatch
// because the alloc-derived genesis hash (empty MPT root) differs from the
// DB-resident genesis hash.
//
// # Build tag gating
//
// The cgo path lives behind `//go:build cgo_reth`. Without that tag,
// RunCgo returns errNotImplemented pointing at Dockerfile.reth (see
// run_stub.go). Local Go builds without libmdbx + librocksdb headers
// remain compilable but cannot exercise the cgo path.
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
//   - temp_sort_cgo.go: Pebble-backed temp sorter for streaming Phase 4
//   - chainspec.go: chainspec JSON writer (built from cfg.Genesis)
//   - header.go: genesis header construction
//   - options.go: Options struct (reserved); buildInjectedAccount helper
package reth
