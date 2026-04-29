# Nethermind Client — Implementation Notes (post-PR#3)

**Branch:** `feat/multi-client-nethermind`
**Date:** 2026-04-28
**Status:** Implemented — ready for review
**Companion to:** [`2026-04-24-nethermind-client-design.md`](./2026-04-24-nethermind-client-design.md) — superseded by direct-write decision below.

**Validated against:** `nethermind/nethermind:1.37.0` (Docker Hub release). The
plan originally pinned upstream/master at SHA `09bd5a2d`, but building from
that SHA tripped a `Microsoft.CodeAnalysis.CSharp 5.3.0` analyzer / running
compiler 5.0.0 mismatch in `Nethermind.Analyzers`. The boot contract
state-actor depends on (`WasProcessed=true` gate, key formats,
`HeaderStore.GetBlockNumberFromBlockNumberDb` 8-byte length check) is
stable across the released line, so smoke + oracle tests run against the
released image.

---

## TL;DR

The 2026-04-24 design picked **Option B: Parity chainspec + ExitOnBlockNumber**. The team moved to **Option A: direct RocksDB write** during planning, and that's what shipped. This file captures the decision, the wire-format gotchas the planning docs got wrong, and the smoke-test evidence that the architecture works at scale.

---

## What changed from the original design

| Aspect | Original spec (Option B) | What shipped (Option A) |
|---|---|---|
| Bootstrap mechanism | Emit Parity chainspec, run `Nethermind.Runner --Init.ExitOnBlockNumber=0` to commit, then re-run for serving | Write the 7 RocksDBs Nethermind reads on boot directly, set `BlockInfos[0].WasProcessed=true` so the loader skips its own genesis pass |
| Cgo | Avoided (run Nethermind as a subprocess) | Required — `linxGnu/grocksdb v1.10.8` against RocksDB 10.10.1 built from source in `Dockerfile.nethermind` |
| Local-build cost | Zero | macOS dev needs Docker; native build behind `-tags cgo_neth` requires librocksdb |
| Scaling ceiling | Bounded by Nethermind's `LoadGenesisBlock` deserializing every alloc into a `Dictionary<Address, ChainSpecAllocation>` (~500 B/account heap → OOM at multi-million scale, upstream issue #7361) | Bounded only by disk: streaming temp Pebble for sorted-by-addrHash account-trie build |
| State-trie writer | N/A (Nethermind builds it from chainspec) | `internal/neth/trie.Builder` wrapping go-ethereum's `StackTrie` with a HalfPath-keyed sink |

The pivot happened because of the chainspec-deserialization scaling cliff. state-actor's reason to exist is multi-million-account devnets; Option B couldn't get there.

---

## Wire-format corrections (planning docs were wrong)

### 1. `blockNumbers` value is fixed-width 8 bytes BE — NOT no-leading-zeros

An earlier revision of `client/nethermind/genesis_cgo.go` (and the deep-feature-planning CCD it was based on) mirrored `Int64Extensions.ToBigEndianSpanWithoutLeadingZeros` (1 byte for genesis). That's the **key** format for `blockInfos/`, but the `blockNumbers/` **value** is 8-byte fixed-width.

Source: `Nethermind.Blockchain.Headers.HeaderStore.GetBlockNumberFromBlockNumberDb` at upstream/master:09bd5a2d, line 103:

```csharp
if (numberSpan.Length != 8)
{
    throw new InvalidDataException($"Unexpected number span length: {numberSpan.Length}");
}
```

Symptom of getting this wrong: Nethermind's BlockTree initializer throws `InvalidDataException("Unexpected number span length: 1")` and falls back to chainspec genesis, silently ignoring the on-disk DB.

**Fixed in commit `6508dad`**: `genesis_cgo.go` now writes 8-byte BE for the blockNumbers value while keeping the no-leading-zeros encoding for the blockInfos key.

### 2. `BaseDbPath` is the data root — NOT a parent of the data root

Geth's convention: data dir = `<base>/geth/chaindata/`. state-actor's geth path mirrors this with a `db/` subdir.

Nethermind's convention: data dir = `<base>/<dbName>/`, no intermediate. So if state-actor writes to `<dbPath>/db/state/`, Nethermind needs `BaseDbPath=<dbPath>/db`, not `BaseDbPath=<dbPath>`.

Symptom: Nethermind opens an empty DB at `<base>/blockInfos/` (which it auto-creates), sees `last_sequence=0`, falls back to chainspec genesis. No error message — your DB is just bypassed.

**Fixed:** state-actor's Nethermind writer now drops the `db/` subdir, so `--db=<path>` and Nethermind's `BaseDbPath=<path>` line up 1:1. The smoke-test config is updated accordingly.

### 3. `stateDBSink.SetStorageNode` is required — not a stub

The original `genesis_alloc_cgo.go` shipped with `SetStorageNode` returning `errors.New("storage trie not supported in Phase B genesis-alloc path")` because the genesis-alloc path didn't write storage. When Phase B's synthetic accounts started exercising contract storage, that placeholder error fired immediately.

Fix: implement `SetStorageNode` to write at the HalfPath storage key (`section=2 + addrHash + path[:8] + pathLen + keccak`).

---

## What ships in PR#3

| Commit | Scope |
|---|---|
| `154a6a6` refactor: introduce Writer interface and extract `client/geth/` | PR#1 prerequisite |
| `76a00d6` refactor(generator): extract RNG primitives into `internal/entitygen/` | PR#2 prerequisite |
| `96ce2cc` foundation encoders — keccak constants, HexPrefix, HalfPath, Account RLP | B3a |
| `7357a0c` block-tree encoders + TrieNode + vendored fixtures | B3b |
| `9ebafe2` `internal/neth/trie.Builder` + `NodeStorage` callback wrapping geth StackTrie | B4 |
| `6803a14` `client/nethermind/` scaffold + `--client=nethermind` CLI dispatch | B5 scaffold |
| `32cecb3` cgo build-tag split + `Dockerfile.nethermind` (RocksDB-from-source) | B5 build infra |
| `edba259` Phase A genesis-only RocksDB writer (empty alloc) | B5 step 1 |
| `6508dad` `blockNumbers` 8-byte BE fix + Phase B genesis-alloc state writes | B5 step 2 + bug fix |
| `6408662` Phase B synthetic-account state writes via entitygen + temp Pebble | B5 final |

## Pipeline summary

```
--accounts=N    --contracts=M    --genesis=g.json
       │              │                │
       └──────┬───────┴────────────────┘
              ▼
      writeSyntheticAccounts
              │
              ├── Phase 1 (random-order):
              │   ├── EOAs    → entitygen.GenerateEOA  → temp Pebble
              │   ├── contracts → entitygen.GenerateContract
              │   │       ├── per-slot: Builder.AddStorageSlot   ← writes State DB at HalfPath storage keys
              │   │       └── FinalizeStorageRoot                ← computes storage root
              │   └── code → Code DB at keccak(code)
              │
              └── Phase 2 (addrHash-sorted via Pebble LSM):
                  └── for each StateAccount:
                      ├── encode as Nethermind RLP
                      └── Builder.AddAccount                     ← writes State DB at HalfPath state keys
                  └── FinalizeStateRoot                          ← global state root
                          │
                          ▼
              header.Root = stateRoot
                          │
                          ▼
              writeGenesisBlockToDBs (metadata-DB writes; blockInfos LAST)
                          ├── headers/      composite key → RLP(header)
                          ├── blocks/       composite key → RLP(block)
                          ├── blockNumbers/ hash(32)      → 8-byte BE  ← FIXED
                          ├── blockInfos/   numKey        → ChainLevelInfo{WasProcessed=true}
                          └── receipts/Blocks CF: composite key → 0xc0
```

Memory: `O(max_slots_per_contract)`. Total entity count is bounded only by the temp Pebble's disk space.

---

## Smoke-test evidence

These were the milestones we hit during implementation; the labels (Phase A / Phase B genesis-alloc / Phase B synthetic) are commit-history landmarks, not currently-tracked work items.

**Empty alloc:**
- `state-actor --client=nethermind --db=/data --accounts=0 --contracts=0` → 7-DB datadir.
- Boot `nethermind/nethermind:1.37.0` → genesis hash matches state-actor's reported hash.

**Genesis-alloc + 100 txs:**
- 3 dev wallets pre-funded via `genesis-funded.json`.
- All 100 dev-mode txs land; chain reaches block 100.

**Synthetic accounts:**
- 100 EOAs + 10 contracts: state root deterministic across re-runs (same `--seed`).
- 100K EOAs + 10K contracts: state root reported by state-actor byte-equals what Nethermind reports for `eth_getBlockByNumber("0x0").stateRoot`.
- 1M EOAs + 100K contracts (max-slots=2048, power-law): 67s generation, 835 MB datadir.
- **6.5M EOAs + 650K contracts (uniform 200–400 storage slots, code-size=512): ~28 min generation, 44 GB datadir.** Booted on `nethermind/nethermind:1.37.0`, ran `spamoor erc20_bloater` for 100 blocks of sustained ERC20 deploy + balance/allowance SSTORE traffic at ~16.5M gas/tx (50% of 30M block limit). Chain mined 100 blocks in ~1m45s under load with no failed txs.

---

## Differential oracle (B6) — passes

`client/nethermind/oracle_test.go` (gated by `//go:build cgo_neth`) runs the full state-actor pipeline against the three CCD-cited Parity chainspec fixtures from `Nethermind.Blockchain.Test.GenesisBuilderTests` and asserts the resulting genesis hash matches Nethermind's own golden values byte-for-byte:

| Fixture | Golden hash | Result |
|---|---|---|
| `empty_accounts_and_storages.json` | `0x61b2253366eab37849d21ac066b96c9de133b8c58a9a38652deae1dd7ec22e7b` | ✅ |
| `empty_accounts_and_codes.json` | `0xfa3da895e1c2a4d2673f60dd885b867d60fb6d823abaf1e5276a899d7e2feca5` | ✅ |
| `hive_zero_balance_test.json` | `0x62839401df8970ec70785f62e9e9d559b256a9a10b343baf6c064747b094de09` | ✅ |

Run via the builder Docker image: `docker build -f Dockerfile.nethermind --target builder -t sab .` then `docker run --rm --entrypoint bash sab -c 'cd /app && go test -tags cgo_neth -run TestDifferentialOracle -v ./client/nethermind/...'`.

### Subtle bits the oracle surfaced

- `hive_zero_balance_test.json` ships with a UTF-8 BOM (saved by an editor that adds one). The loader strips `EF BB BF` before `encoding/json` sees the input.
- `empty_accounts_and_codes.json`'s `gasLimit` is `0x0bebc200` — leading-zero hex digits trip go-ethereum's `hexutil.Uint64`. The oracle's parser uses `big.Int.SetString` which accepts them.
- Nethermind keeps an alloc account when the chainspec specifies a `balance` field at all, even if it's `0x0` (the `precompile that has zero balance` test name was the cue). The EIP-161 emptiness filter checks balance **presence**, not its value.

## Known gaps (not blocking PR#3)

- **Streaming snapshot writes** — the geth path uses an async snapshot-writer goroutine; the Nethermind path is single-goroutine. At 5M+500K scale CPU sits around 30-71% on one core, so there's headroom to parallelize when needed.
- **WriteBatch flush threshold (16 MiB)** is a starting point. Worth re-tuning by benchmarking on hosts with different fsync latencies.
