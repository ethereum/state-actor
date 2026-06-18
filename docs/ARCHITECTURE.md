# Architecture

This document explains the internal architecture of State Actor.

## Overview

State Actor generates Ethereum state in three phases:

1. **Account Generation** — Create EOAs and contracts with storage
2. **State Root Computation** — Build StackTrie and compute root
3. **Database Writing** — Write snapshot layer and genesis block

## Component Diagram

```
┌──────────────────────────────────────────────────────────────────────────┐
│                              CLI Layer                                    │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │                         main.go                                     │ │
│  │  • Parse flags                                                      │ │
│  │  • Synthesize chainspec from --chain-id/--fork/--gas-limit/...      │ │
│  │  • Load optional --spec YAML                                        │ │
│  │  • Initialize generator                                             │ │
│  │  • Print statistics                                                 │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                           Genesis Package                                 │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │                      genesis/genesis.go                             │ │
│  │  • LoadGenesis() — parse JSON file                                  │ │
│  │  • ToStateAccounts() — convert alloc to StateAccount               │ │
│  │  • GetAllocStorage() — extract storage maps                        │ │
│  │  • GetAllocCode() — extract contract bytecode                      │ │
│  │  • WriteGenesisBlock() — write block header + metadata             │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                          Generator Package                                │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │                    generator/generator.go                           │ │
│  │                                                                     │ │
│  │  ┌────────────────────┐         ┌────────────────────────┐         │ │
│  │  │ Spec PreAlloc      │         │ internal/autofill.Plan │         │ │
│  │  │                    │         │                        │         │ │
│  │  │ • Explicit / named │         │ • PlanForBudget(b)     │         │ │
│  │  │   / position-derived│        │ • DrawEOA(rng)         │         │ │
│  │  │ • Templates (erc20,│         │ • DrawContract(rng)    │         │ │
│  │  │   raw, eoa, 7702)  │         │   20/10/70 mainnet     │         │ │
│  │  └─────────┬──────────┘         └───────────┬────────────┘         │ │
│  │            │                                │                      │ │
│  │            └────────────────┬───────────────┘                      │ │
│  │                             ▼                                       │ │
│  │  ┌─────────────────────────────────────────────────────────────┐   │ │
│  │  │              Streaming Trie + Sort                          │   │ │
│  │  │  • internal/streamsort: out-of-core key sort                │   │ │
│  │  │  • internal/streamingtrie: O(depth) RAM, hash on the fly    │   │ │
│  │  │  • Per-account storage roots spliced into StateAccount.Root │   │ │
│  │  │  • Global state root emitted in Phase 2                     │   │ │
│  │  └─────────────────────────────────────────────────────────────┘   │ │
│  │                             │                                       │ │
│  │                             ▼                                       │ │
│  │  ┌─────────────────────────────────────────────────────────────┐   │ │
│  │  │       Per-client Writer (client/<name>/)                    │   │ │
│  │  │  • generator.Writer interface                               │   │ │
│  │  │  • geth: pure-Go Pebble. reth/besu/nethermind: cgo.         │   │ │
│  │  └─────────────────────────────────────────────────────────────┘   │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                  Client-native DB (per --client)                          │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │                    Snapshot Layer (geth MPT)                        │ │
│  │  Key: a + hash(addr)           Value: full StateAccount RLP        │ │
│  │  Key: o + hash(addr) + hash(k) Value: RLP(trimmed_value)           │ │
│  │  Key: c + hash(code)           Value: bytecode                     │ │
│  │  Key: SnapshotRoot             Value: state_root                   │ │
│  ├─────────────────────────────────────────────────────────────────────┤ │
│  │            Flat State (geth binary-trie / EIP-7864)                 │ │
│  │  Key: vX + stem(31)            Value: stem blob (group payloads)   │ │
│  │  Key: vN + path(<=31)          Value: serialized trie node         │ │
│  │  Key: c + hash(code)           Value: bytecode                     │ │
│  ├─────────────────────────────────────────────────────────────────────┤ │
│  │                   Genesis Metadata (geth)                           │ │
│  │  Key: h + num + hash           Value: block_header_rlp             │ │
│  │  Key: b + num + hash           Value: block_body_rlp               │ │
│  │  Key: H + num                  Value: canonical_hash               │ │
│  │  Key: LastBlock                Value: head_block_hash              │ │
│  │  Key: LastHeader               Value: head_header_hash             │ │
│  │  Key: ethereum-config-...      Value: chain_config_json            │ │
│  ├─────────────────────────────────────────────────────────────────────┤ │
│  │  reth: MDBX state tables + RocksDB history + nippy-jar static_files│ │
│  │  besu: single RocksDB w/ 8 Bonsai column families + chainspec.json │ │
│  │  nethermind: 7 RocksDB instances + parity-format chainspec sidecar │ │
│  │  ethrex: single RocksDB w/ 20 CFs + metadata.json + genesis sidecar│ │
│  └─────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

## Data Flow

### 1. Initialization

State Actor synthesises a chainspec from CLI flags (`--chain-id`, `--fork`,
`--gas-limit`, `--timestamp`, `--extra-data`) and optionally loads a YAML
state-spec via `--spec` (see [`SPEC.md`](SPEC.md)).

```go
// Synthesize genesis chainspec from flags
g, _ := stategenesis.BuildSynthetic(fork, chainID, gasLimit, timestamp, extraData)

// Optionally parse + validate a YAML spec
specDoc, _ := spec.ParseFile(specPath)
specDoc.Validate(templates.UserVisibleNames())

// Resolve spec entities → PreAlloc (explicit / name-derived / position-derived
// addresses; storage synthesized from approximate_size_bytes via per-client
// sizecal factors).
preAlloc, _, _ := specbuild.Build(specDoc, specbuild.BuildOptions{Seed: seed, Client: client})

config := generator.Config{
    DBPath:     dbPath,
    Genesis:    g,
    PreAlloc:   preAlloc,                  // spec entities, written first
    AutoFill:   autofill.PlanForBudget(b), // mainnet-shaped synthetic fill (nil = none)
    TargetSize: b,                         // on-disk stop condition
    // ... other config
}
```

The PreAlloc list captures every spec entity; if `--target-size` is set,
`internal/autofill.Plan` drives the synthetic top-up with mainnet-shaped
20 / 10 / 70 proportions (account-trie / bytecode / storage) on top until
the target is reached.

### 2. Account Generation

The generator emits entities in three layers, in order:

```go
// 1. Canonical syscontracts (BeaconRoots, HistoryStorage, WithdrawalQueue,
//    ConsolidationQueue, DepositContract) at their hardcoded addresses.
syscontracts.AddCanonicalSystemContracts(&config)

// 2. Spec PreAlloc entities (explicit / name-derived / position-derived).
//    Materialized into config.GenesisAccounts / config.GenesisCode by
//    Config.Validate(), then streamed by per-client writers.
for _, pe := range config.PreAlloc { /* ... */ }

// 3. Auto-fill synthetic entities (only when config.AutoFill != nil — i.e.
//    when --target-size is set). The Plan's NumEOAs / NumContracts are
//    pre-derived from the top-up budget; each Draw* method emits one
//    entity using the canonical entitygen RNG sequence so all 5 client
//    emission sites (geth-MPT, geth-bintrie, reth, besu, nethermind)
//    produce byte-identical entities for the same seed.
if plan := config.AutoFill; plan != nil {
    for i := 0; i < plan.NumEOAs && !targetReached; i++ {
        acc := plan.DrawEOA(rng)
        // ... write to streamsort sorter ...
    }
    for i := 0; i < plan.NumContracts && !targetReached; i++ {
        contract := plan.DrawContract(rng)
        // ... write to streamsort sorter ...
    }
}
```

Per-client emission sites stop early when the on-disk size hits
`config.TargetSize` (sampled via `dirSize` for reth/nethermind, projected
via per-entity byte estimates for geth/besu).

### 3. State Root Computation

StackTrie requires sorted keys for correct root. The streaming pipeline
sorts on disk via `internal/streamsort` so the trie hasher sees keys in
hash order without buffering the full account set in memory:

```go
// Phase 1 has already written entities to a Pebble sorter keyed by
// keccak256(address). Phase 2 iterates in sorted order:
sorter.Iterate(func(addrHash []byte, entityBlob []byte) error {
    // For each account with storage: build per-account storage trie
    // via internal/streamingtrie, splice the storage root into the
    // StateAccount, then re-encode the FULL StateAccount RLP (not
    // SlimAccountRLP — geth's PathDB requires the full form).
    accountTrie.Update(addrHash, fullStateAccountRLP)
    return nil
})

stateRoot := accountTrie.Hash()
```

### 4. Database Writing

Each `--client` owns its on-disk format and ships its own writer adapter
in `client/<name>/`. The shared streaming pipeline (`internal/streamsort`
+ `internal/streamingtrie`) feeds the per-client writer one account at
a time over a sorted key stream — no in-memory account list, no
configurable worker pool / batch size at the generator level.

- **geth-MPT** (`client/geth/state_writer.go:44-55`): two-phase
  streamsort. Phase 1 collects entities into a sorted Pebble store
  keyed by `addrHash`; Phase 2 iterates the sorted output, builds per-
  account storage tries via `internal/streamingtrie`, writes the
  production Pebble in keccak order, and feeds the outer account trie.
- **geth-bintrie** (`generator/generator.go:140-200`): producer-
  consumer pipeline. The producer emits accounts/contracts into a
  channel; the consumer writes to a temp Pebble + streams a binary
  StackTrie. Phase 2 reads the temp DB to produce the genesis state
  root + stem-blob flat state.
- **reth** (`client/reth/run_cgo.go:81-194`): cgo + libmdbx. Streams
  EOAs in 100 K batches and contracts in 1 K batches into MDBX state
  tables, with a per-batch `dirSize` sample driving the target-size
  early stop.
- **besu** (`client/besu/state_writer_cgo.go:60-160`): cgo + librocksdb.
  Single RocksDB with 8 Bonsai column families; per-entity raw-bytes
  accumulator drives the target-size stop.
- **nethermind** (`client/nethermind/{run_cgo,entitygen_cgo}.go`): cgo
  + grocksdb. Seven RocksDB instances; periodic `dirSize` sample
  (every 100 contracts) drives the target-size stop.
- **ethrex** (`client/ethrex/run_cgo.go`): cgo + grocksdb. Single
  RocksDB with 20 column families. Account and storage trie nodes
  are encoded via `internal/ethrex`'s path-keyed trie codec (two rows
  per leaf: one full-path row, one nibble-path row). Writes
  `metadata.json` + `ethrex-genesis.json` sidecars. Behind the
  `cgo_ethrex` build tag.

Each adapter implements the `generator.Writer` interface
(`WriteAccount`, `WriteStorage`, `WriteCode`, `SetStateRoot`, …); the
generator core only sees the abstract Writer surface.

### 5. Genesis Block Writing

When genesis is provided:

```go
// Create block header with state root
header := &types.Header{
    Number:     big.NewInt(0),
    Root:       stateRoot,  // From step 3
    // ... other fields from genesis
}

block := types.NewBlock(header, ...)

// Write to database
rawdb.WriteBlock(batch, block)
rawdb.WriteCanonicalHash(batch, block.Hash(), 0)
rawdb.WriteHeadBlockHash(batch, block.Hash())
rawdb.WriteChainConfig(batch, block.Hash(), genesis.Config)
```

## Key Design Decisions

### Snapshot Layer Only

State Actor writes only to the snapshot layer, not the full MPT trie. Geth can regenerate the trie from snapshots if needed. This significantly improves write performance.

### Sort by Hash for StackTrie

StackTrie requires keys in sorted order to produce correct roots. We sort:
- Accounts by `keccak256(address)`
- Storage slots by `keccak256(slot)`

### Auto-fill Distribution

Synthetic top-up (`internal/autofill`) emits a fixed mainnet-shaped split:
**20 % account-trie / 10 % bytecode / 70 % contract storage** (constants
in [`internal/sizecal/factors.go`](../internal/sizecal/factors.go)).
Per-contract code is a truncated normal in `[1 KiB, 24 KiB]` centered at
5 KiB (`MeanContractCode`); per-contract storage size is a truncated
normal in `[1 KiB, 100 MiB]` whose mean is budget-derived — typically
~35 KiB at any target scale. EOAs randomize balance (90 % non-zero),
nonce (always non-zero), and EIP-7702 delegation (30 %) independently.

Spec-loaded entities (`--spec`) are separate: their distribution comes
from the YAML schema (per-entity `approximate_size_bytes` resolved via
`internal/sizecal.SlotsForBytes`) and is independent of the auto-fill
shape above.

### Genesis Account Preservation

When merging genesis accounts, we preserve their exact addresses (not random). This ensures validator addresses, system contracts, and prefunded accounts work correctly.

### Deep-Branch Phantom Injection

Deep-branch accounts use phantom entries to force branch nodes at every nibble depth in a storage trie. For a legitimate slot with trie key `T = keccak256(pad32(slotIndex))`, we construct `D` phantom keys where phantom `d` matches `T` on nibbles `[0..d-1]` but differs at nibble `d`. These are written to the snapshot via `WriteRawStorage` (bypassing `keccak256`) and inserted directly into the StackTrie. The legitimate slot's `SLOAD` path traverses all `D` branch nodes.

### Streaming Pipeline (no worker pool)

The generator streams entities through `internal/streamsort` (out-of-core
key sort) into `internal/streamingtrie` (O(depth)-RAM hasher) into the
per-client writer. Total RAM stays bounded (~2 GB peak) regardless of
total state size; the bottleneck is the client DB's compaction/write
throughput, not generator-side batching. No worker pool, no configurable
batch size at the generator level. Per-client writers may internally
batch (e.g., reth's 100 K EOA + 1 K contract MDBX-txn batches at
`client/reth/run_cgo.go:25`) for FFI/txn-amortization reasons, but those
are implementation details of the writer adapter.

## Client Adapters

State writers are pluggable via the `generator.Writer` interface. Each
client/<name>/ package owns its target client's on-disk format end-to-end:
key encoding, batching, genesis-block wire format, and any client-specific
metadata. The generator core only sees the abstract Writer surface
(WriteAccount, WriteStorage, WriteCode, SetStateRoot, …).

Clients register themselves as the default writer factory via init():

```go
import _ "github.com/ethereum/state-actor/client/geth"
gen, err := generator.New(cfg) // uses geth's Pebble writer
```

To select a factory explicitly (or to support future clients alongside geth):

```go
import "github.com/ethereum/state-actor/client/geth"
gen, err := generator.NewWithWriter(cfg, geth.NewWriterFactory())
```

Today's client adapters:

- `client/geth/` — the original, pure-Go Pebble writer producing geth's
  snapshot layer + PathDB metadata.
- `client/reth/` — cgo + libmdbx writer producing MDBX state tables +
  RocksDB history + nippy-jar `static_files/` segments + a reth-format
  `chainspec.json` sidecar. Behind the `cgo_reth` build tag.
- `client/besu/` — cgo + librocksdb writer producing a single RocksDB
  with 8 Bonsai column families (default + `BLOCKCHAIN` +
  `ACCOUNT_INFO_STATE` + `CODE_STORAGE` + `ACCOUNT_STORAGE_STORAGE` +
  `TRIE_BRANCH_STORAGE` + `TRIE_LOG_STORAGE` + `VARIABLES`) plus a
  besu-format chainspec sidecar. Behind the `cgo_besu` build tag.
- `client/nethermind/` — cgo + grocksdb writer producing seven RocksDB
  instances (`state`, `code`, `blocks`, `headers`, `blockNumbers`,
  `blockInfos`, `receipts`) plus a parity-format chainspec sidecar.
  Behind the `cgo_neth` build tag.
- `client/ethrex/` — cgo + grocksdb writer producing a single RocksDB
  with 20 column families (full list in `internal/ethrex/constants.go`)
  using ethrex's own path-keyed trie codec (`internal/ethrex/`). Two
  rows written per leaf (full-path + nibble-path). Sidecars:
  `metadata.json` (schema_version=2) and `ethrex-genesis.json` (full
  genesis JSON for `ethrex --network <path>`). Behind the `cgo_ethrex`
  build tag.

The Nethermind adapter takes a different route from the others: instead of
writing a chainspec for the client to consume, it writes the seven RocksDB
instances Nethermind reads on boot directly. This bypasses Nethermind's
`LoadGenesisBlock` step (which would deserialize every alloc account into
a `Dictionary<Address, ChainSpecAllocation>` — fine at small scale, OOMs
at multi-million-account scale per upstream issue #7361). The boot gate
is `BlockInfos[0].WasProcessed=true`.

Default `go build` produces stubs for the cgo writers that point users at
the corresponding per-client Dockerfile.

## File structure

```
state-actor/
├── main.go                          # CLI entry point + flag parsing
├── AGENTS.md                        # Agent-facing entry doc
├── README.md                        # Human-facing entry doc
├── client/
│   ├── geth/                        # Pure-Go Pebble writer (default)
│   ├── reth/                        # cgo + libmdbx writer (cgo_reth build tag)
│   ├── besu/                        # cgo + librocksdb writer (cgo_besu build tag)
│   ├── nethermind/                  # cgo + grocksdb writer (cgo_neth build tag)
│   └── ethrex/                      # cgo + grocksdb writer, 20 CFs (cgo_ethrex build tag)
├── generator/                       # Core generation pipeline + Writer interface
├── genesis/                         # Client-neutral chainspec types + builder
├── internal/
│   ├── spec/                        # YAML --spec parser + validator
│   ├── specbuild/                   # Spec → PreAlloc resolver (addresses, templates)
│   ├── templates/                   # Spec templates (erc20, ...)
│   ├── sizecal/                     # Byte-budget → slot-count via single global bytesPerSlot constant
│   ├── streamingtrie/               # O(depth) streaming trie hasher
│   ├── streamsort/                  # Out-of-core key sort for the streaming trie
│   ├── entitygen/                   # Synthetic-fill entity generator
│   ├── clientpolicy/                # Per-client flag validation (--binary-trie, --target-size, --fork)
│   ├── syscontracts/                # Canonical EIP-4788/2935/7002/7251 deployments
│   ├── genesisheader/               # Cross-client genesis header builder
│   ├── oracle/                      # Reproduce-from-config RNG (devkeys + reproduce)
│   ├── reth/                        # Reth-side codec (MDBX wire format)
│   ├── neth/                        # Nethermind-side helpers
│   ├── ethrex/                      # ethrex path-keyed trie codec + RocksDB helpers
│   ├── engineapi/                   # Mock CL engine-API driver (besu / nethermind boot)
│   ├── e2e_testing/                 # Shared per-client TestE2ESuite phases + checks + RPC oracle
│   ├── rpcprobe/                    # Waitfor-RPC + JSON-RPC helpers
│   └── testhex/                     # Hex helpers for tests
├── integration/                     # Kurtosis Starlark + geth-wrapper.sh
├── examples/                        # Curated spec YAMLs (see examples/README.md)
└── docs/                            # SKILL.md, SPEC.md, RUNBOOK.md, ARCHITECTURE.md, KURTOSIS.md
```

## Cross-client determinism

State Actor guarantees that the same `--seed`, the same `--spec`, and the same client-policy preamble produce **the same genesis state root** across all five MPT clients (geth / reth / besu / nethermind / ethrex). This is the load-bearing invariant the project exists to enable.

The mechanism is three-layered:

- **Deterministic address derivation.** Spec address modes — explicit, name-derived (`keccak256(BE_u64(seed) || utf8(name))[12:]`), position-derived (same but with `anon-N`) — are pure functions of the spec input. Pinned at unit level by `internal/specbuild/derive_test.go:TestResolveAddressDeterministicAcrossRuns`.
- **Single global byte-budget constant.** `--spec`'s `approximate_size_bytes` is converted to a synthesised slot count via the global `bytesPerSlot` constant in [`internal/sizecal/factors.go`](../internal/sizecal/factors.go) — identical across all five clients, which is precisely what makes the cross-client root match. The CI invariance gate calls `sizecal.NewFixed(64)` to decouple test sizing from the production `Default()`, so a drift in either side can't silently mask the other.
- **Canonical syscontract preamble.** Every per-client writer must run `syscontracts.AddCanonicalSystemContracts(&cfg)` before producing state. The five EIP-mandated system contracts (BeaconRoots, HistoryStorage, WithdrawalQueue, ConsolidationQueue, DepositContract) must exist at their canonical addresses; without them besu refuses to boot and the other four clients compute a different root.

The CI keystone job `cross-client-genesis-root` (defined in `.github/workflows/ci.yml`, exercising `examples/full-matrix-spec-feature.yaml`) re-asserts the invariant on every PR. When a divergence appears, the most likely cause is calibration drift (`internal/sizecal/`) or a missing syscontract preamble; less common but possible is per-client codec drift (`internal/reth/`, `internal/neth/`, etc.).

## Performance characteristics

State Actor's writer pipelines are streaming: total RAM stays bounded
(~2 GB peak for the spec-driven path, regardless of total state size)
because the streaming trie hasher (`internal/streamingtrie`) consumes one
account at a time over a sorted key stream (`internal/streamsort`),
emitting trie nodes as the stream advances rather than building the
trie in memory. End-to-end throughput is dominated by the chosen
client's on-disk format — Pebble compaction (geth), MDBX random-write
IOPS (reth), RocksDB compaction (besu / nethermind / ethrex). Numbers vary by
host and would rot fast; benchmark on your own hardware with
`--benchmark --verbose` if you need a concrete figure.
