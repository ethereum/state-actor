// Package besu writes a fully-bootable Hyperledger Besu Bonsai RocksDB
// database directly via cgo+grocksdb, bypassing Besu's GenesisState
// in-memory recompute on first boot.
//
// # Approach
//
// Open one RocksDB instance under <datadir>/database/ with the 8 column
// families Besu Bonsai expects (default + BLOCKCHAIN + ACCOUNT_INFO_STATE +
// CODE_STORAGE + ACCOUNT_STORAGE_STORAGE + TRIE_BRANCH_STORAGE +
// TRIE_LOG_STORAGE + VARIABLES). Stream synthetic accounts through a temp
// Pebble DB (Phase 1), iterate them in addrHash-sorted order to feed the
// Bonsai path-keyed trie builder (Phase 2), write per-account flat state +
// per-account storage trie nodes via NodeSink, then assemble the genesis
// block (header / body / receipts / canonical hash / TD), and finally write
// VARIABLES["chainHeadHash"] last.
//
// The result boots `hyperledger/besu:25.11.0` against the produced datadir
// with `--genesis-state-hash-cache-enabled` — Besu reads the stored stateRoot
// directly (the synthetic state can't match a recompute from the genesis
// JSON's alloc map by definition), validates the genesis block hash, and
// starts from block 0 ready.
//
// Why pin to 25.11.0 specifically: Besu 26.x removed the standalone
// --miner-enabled / --miner-coinbase flags (post-merge consolidation —
// block production is now Engine-API-driven via a paired consensus
// client). Without those flags, mining stops working against custom
// genesis configs. Sticking with 25.11.0 keeps single-binary block
// production until the Engine-API path is plumbed.
//
// When the Engine-API path runs against a 26.x Besu, keep p2p enabled: Besu
// accepts engine_forkchoiceUpdated on an isolated node only once its
// synchronizer has registered the post-merge head as in-sync, which it does
// only with p2p on — --p2p-enabled=false makes Besu answer SYNCING.
//
// # Build
//
// state-actor's Besu path is **Docker-only**. The cgo_besu build tag gates
// all grocksdb-importing files; vanilla `go build` (the local default)
// compiles the stub at run_stub.go which returns a clear error directing
// the user at the Dockerfile.
//
//	docker build -f Dockerfile.besu -t state-actor-besu .
//	docker run --rm -v $PWD/_artifacts:/data state-actor-besu \
//	  --client=besu --db=/data --target-size=500MB --seed=42
//
// # Pinned target
//
// internal/besu/'s RLP shapes, key encodings, and trie/builder.go's
// Bonsai path-keyed MPT mirror Besu upstream tag 26.5.0 (May 2026; the
// Bonsai schema is stable across the released line, so the slightly older
// 25.11.0 boot image accepts what we write). End-to-end smoke and the
// differential oracle run against the released image
// hyperledger/besu:25.11.0 — the boot contract Besu enforces (chainHeadHash
// pointer, genesis block hash match, Bonsai trie path-keyed encoding) is
// stable across the released line.
package besu
