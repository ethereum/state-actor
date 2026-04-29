// Package nethermind writes a fully-bootable Nethermind RocksDB database
// directly, bypassing Nethermind's chainspec loader.
//
// # Approach
//
// Run() opens the RocksDB instances directly under <datadir>/ that
// Nethermind expects (state, code, blocks, headers, blockNumbers,
// blockInfos, receipts), populates State+Code via the
// writeSyntheticAccounts / writeGenesisAllocAccounts dispatch, then
// assembles the genesis block tree (header / block / blockNumbers /
// blockInfos with WasProcessed=true / empty receipts at row 0).
// Booting nethermind against the produced datadir starts at block 0
// ready — no init phase, no chainspec preallocation pass.
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
//	  --client=nethermind --db=/data/neth --accounts=1000 --seed=42
//
// # Pinned target
//
// internal/neth/'s RLP shapes and HalfPath layouts mirror Nethermind
// upstream/master at SHA 09bd5a2d (2026-04-26). End-to-end smoke and the
// Tier 2 differential oracle run against the released image
// nethermind/nethermind:1.37.0 — the boot contract Nethermind enforces
// (WasProcessed=true gate, key formats, 8-byte blockNumbers values) is
// stable across the released line, so the released image is what the
// `make smoke-nethermind` and `make test-nethermind-oracle` targets
// drive against.
package nethermind
