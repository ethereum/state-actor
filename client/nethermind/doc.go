// Package nethermind writes a fully-bootable Nethermind RocksDB database
// directly, bypassing Nethermind's chainspec loader.
//
// # Approach
//
// Run() opens the RocksDB instances directly under <datadir>/ that
// Nethermind expects (state, code, blocks, headers, blockNumbers,
// blockInfos, receipts) plus the flat-state column DB (see below), populates
// the flat DB (trie nodes + Account/Storage rows) and Code via
// writeSyntheticAccounts (which handles synthetic generation, genesis-
// alloc accounts, AND spec-PreAlloc entities via the streaming Phase 0),
// then assembles the genesis block tree (header / block / blockNumbers /
// blockInfos with WasProcessed=true / empty receipts at row 0).
// Booting nethermind against the produced datadir starts at block 0
// ready — no init phase, no chainspec preallocation pass.
//
// # State layout
//
// The writer emits Nethermind's flat-state layout. An eighth RocksDB — a
// *column* DB at <datadir>/flat — holds the state as flat Account/Storage leaf
// rows plus the Merkle-Patricia trie (relocated into four node column
// families), with three Metadata-CF markers (Layout / SlotEncoding /
// CurrentState). The CurrentState marker (block 0 ‖ genesis state root) makes
// Nethermind ≥1.39.0 detect and serve the DB as its "flat" backend. The trie is
// NOT removed — the state root is still computed by the same StackTrie, so the
// golden root is unchanged. The legacy `state` RocksDB is created empty
// (Nethermind's backend detection inspects it) and is otherwise unused.
//
// Booting a flat datadir requires `--FlatDb.Enabled=true`; without it
// Nethermind selects patricia and finds an empty `state` DB. The flat layout is
// byte-mirrored from Nethermind in internal/neth/flat; the marker write order
// (data flushed → Layout/SlotEncoding/CurrentState in one synced batch →
// block-tree write with blockInfos last) keeps the boot gates truthful across a
// crash.
//
// # Build
//
// state-actor's Nethermind path is **Docker-only**. The cgo_neth build
// tag gates all grocksdb-importing files; vanilla `go build` (the local
// default) compiles the stub at run_stub.go which returns a clear error
// directing the user at the Dockerfile.
//
//	docker build -f Dockerfile.nethermind -t state-actor-nethermind .
//	docker run --rm -v $PWD/_artifacts:/data state-actor-nethermind \
//	  --client=nethermind --db=/data/neth --target-size=500MB --seed=42
//
// # Pinned target
//
// internal/neth (and internal/neth/flat) mirror Nethermind release tag 1.39.0
// (commit 14aca2c5202c48a6a46c8d928fe7ed8a898d253e); the pin is recorded in
// internal/neth/constants.go as PinnedNethermindVersion. End-to-end smoke and
// the differential oracle run against the released image
// nethermind/nethermind:1.39.0. The boot contract Nethermind enforces
// (WasProcessed=true gate, key formats, 8-byte blockNumbers values, and the
// flat CurrentState marker) is what the smoke/oracle/e2e targets drive against.
package nethermind
