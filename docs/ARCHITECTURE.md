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
│  │  ┌───────────────┐  ┌───────────────┐  ┌───────────────┐           │ │
│  │  │ Account Gen   │  │ Contract Gen  │  │ Storage Gen   │           │ │
│  │  │               │  │               │  │               │           │ │
│  │  │ • Random addr │  │ • Random addr │  │ • Distribution│           │ │
│  │  │ • Balance     │  │ • Code        │  │ • Key/value   │           │ │
│  │  │ • Nonce       │  │ • Storage     │  │ • RLP encode  │           │ │
│  │  └───────┬───────┘  └───────┬───────┘  └───────┬───────┘           │ │
│  │          │                  │                  │                    │ │
│  │          └──────────────────┼──────────────────┘                    │ │
│  │                             ▼                                       │ │
│  │  ┌─────────────────────────────────────────────────────────────┐   │ │
│  │  │                    StackTrie Builder                        │   │ │
│  │  │  • Sort accounts by hash(address)                           │   │ │
│  │  │  • Sort storage by hash(slot)                               │   │ │
│  │  │  • Compute storage roots per account                        │   │ │
│  │  │  • Compute global state root                                │   │ │
│  │  └─────────────────────────────────────────────────────────────┘   │ │
│  │                             │                                       │ │
│  │                             ▼                                       │ │
│  │  ┌─────────────────────────────────────────────────────────────┐   │ │
│  │  │                   Batch Writer                              │   │ │
│  │  │  • Parallel workers                                         │   │ │
│  │  │  • Configurable batch size                                  │   │ │
│  │  │  • Write to Pebble                                          │   │ │
│  │  └─────────────────────────────────────────────────────────────┘   │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                         Pebble Database                                   │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │                    Snapshot Layer                                   │ │
│  │  Key: a + hash(addr)           Value: SlimAccountRLP               │ │
│  │  Key: o + hash(addr) + hash(k) Value: RLP(trimmed_value)           │ │
│  │  Key: c + hash(code)           Value: bytecode                     │ │
│  │  Key: SnapshotRoot             Value: state_root                   │ │
│  ├─────────────────────────────────────────────────────────────────────┤ │
│  │                   Genesis Metadata                                  │ │
│  │  Key: h + num + hash           Value: block_header_rlp             │ │
│  │  Key: b + num + hash           Value: block_body_rlp               │ │
│  │  Key: H + num                  Value: canonical_hash               │ │
│  │  Key: LastBlock                Value: head_block_hash              │ │
│  │  Key: LastHeader               Value: head_header_hash             │ │
│  │  Key: ethereum-config-...      Value: chain_config_json            │ │
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
    DBPath:      dbPath,
    Genesis:     g,
    PreAlloc:    preAlloc,                 // spec entities, written first
    NumAccounts: synthAccounts,            // synthetic-fill EOAs
    NumContracts: synthContracts,          // synthetic-fill contracts
    // ... other config
}
```

The PreAlloc list captures every spec entity; the synthetic-fill loop
(`--accounts` / `--contracts`) runs on top until `--target-size` is reached.

### 2. Account Generation

The generator creates accounts in memory before writing:

```go
// Genesis accounts first (preserve exact addresses)
for addr, acc := range config.GenesisAccounts {
    // Include at exact address
}

// Then generated accounts
for i := 0; i < config.NumAccounts; i++ {
    // Random address, balance, nonce
}

// Then generated contracts
for i := 0; i < config.NumContracts; i++ {
    // Random address, code, storage slots
    // Storage count from distribution
}
```

### 3. State Root Computation

StackTrie requires sorted keys for correct root:

```go
// Sort all accounts by hash(address)
sort.Slice(allAccounts, func(i, j int) bool {
    return bytes.Compare(
        allAccounts[i].addrHash[:],
        allAccounts[j].addrHash[:],
    ) < 0
})

// For each account with storage:
//   1. Sort storage keys by hash(slot)
//   2. Build storage trie
//   3. Get storage root
//   4. Update account.Root

// Build account trie
for _, acc := range allAccounts {
    accountTrie.Update(acc.addrHash[:], slimAccountRLP)
}

stateRoot := accountTrie.Hash()
```

### 4. Database Writing

Parallel batch writers for throughput:

```go
// Worker pool for batch commits
for i := 0; i < config.Workers; i++ {
    go func() {
        for batch := range batchChan {
            batch.Write()
        }
    }()
}

// Write all data
for _, acc := range allAccounts {
    // Write storage slots
    for key, value := range acc.storage {
        batch.Put(storageKey(acc.addrHash, keyHash), rlpValue)
    }
    // Write code
    if len(acc.code) > 0 {
        batch.Put(codeKey(acc.codeHash), acc.code)
    }
    // Write account
    batch.Put(accountKey(acc.addrHash), slimAccountRLP)
    
    // Flush batch when full
    if batchCount >= config.BatchSize {
        batchChan <- batch
        batch = db.NewBatch()
    }
}
```

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

### Power-Law Distribution

Real Ethereum state follows a power-law distribution: a few contracts (Uniswap, etc.) have millions of slots while most have very few. We use Pareto distribution to simulate this.

### Genesis Account Preservation

When merging genesis accounts, we preserve their exact addresses (not random). This ensures validator addresses, system contracts, and prefunded accounts work correctly.

### Deep-Branch Phantom Injection

Deep-branch accounts use phantom entries to force branch nodes at every nibble depth in a storage trie. For a legitimate slot with trie key `T = keccak256(pad32(slotIndex))`, we construct `D` phantom keys where phantom `d` matches `T` on nibbles `[0..d-1]` but differs at nibble `d`. These are written to the snapshot via `WriteRawStorage` (bypassing `keccak256`) and inserted directly into the StackTrie. The legitimate slot's `SLOAD` path traverses all `D` branch nodes.

### Parallel Batch Writers

Pebble performs best with parallel batch commits. We use a worker pool to maximize throughput while maintaining ordering within batches.

## Client Adapters

State writers are pluggable via the `generator.Writer` interface. Each
client/<name>/ package owns its target client's on-disk format end-to-end:
key encoding, batching, genesis-block wire format, and any client-specific
metadata. The generator core only sees the abstract Writer surface
(WriteAccount, WriteStorage, WriteCode, SetStateRoot, …).

Clients register themselves as the default writer factory via init():

```go
import _ "github.com/nerolation/state-actor/client/geth"
gen, err := generator.New(cfg) // uses geth's Pebble writer
```

To select a factory explicitly (or to support future clients alongside geth):

```go
import "github.com/nerolation/state-actor/client/geth"
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
│   └── nethermind/                  # cgo + grocksdb writer (cgo_neth build tag)
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
│   ├── engineapi/                   # Mock CL engine-API driver (besu / nethermind boot)
│   ├── e2e_testing/                 # Shared per-client TestE2ESuite phases + checks + RPC oracle
│   ├── rpcprobe/                    # Waitfor-RPC + JSON-RPC helpers
│   └── testhex/                     # Hex helpers for tests
├── integration/                     # Kurtosis Starlark + geth-wrapper.sh
├── examples/                        # Curated spec YAMLs (see examples/README.md)
└── docs/                            # SKILL.md, SPEC.md, RUNBOOK.md, ARCHITECTURE.md, KURTOSIS.md
```

## Cross-client determinism

State Actor guarantees that the same `--seed`, the same `--spec`, and the same client-policy preamble produce **the same genesis state root** across all four MPT clients (geth / reth / besu / nethermind). This is the load-bearing invariant the project exists to enable.

The mechanism is three-layered:

- **Deterministic address derivation.** Spec address modes — explicit, name-derived (`keccak256(BE_u64(seed) || utf8(name))[12:]`), position-derived (same but with `anon-N`) — are pure functions of the spec input. Pinned at unit level by `internal/specbuild/derive_test.go:TestResolveAddressDeterministicAcrossRuns`.
- **Single global byte-budget constant.** `--spec`'s `approximate_size_bytes` is converted to a synthesised slot count via the global `bytesPerSlot` constant in [`internal/sizecal/factors.go`](../internal/sizecal/factors.go) — identical across all four clients, which is precisely what makes the cross-client root match. The CI invariance gate calls `sizecal.NewFixed(64)` to decouple test sizing from the production `Default()`, so a drift in either side can't silently mask the other.
- **Canonical syscontract preamble.** Every per-client writer must run `syscontracts.AddCanonicalSystemContracts(&cfg)` before producing state. The five EIP-mandated system contracts (BeaconRoots, HistoryStorage, WithdrawalQueue, ConsolidationQueue, DepositContract) must exist at their canonical addresses; without them besu refuses to boot and the other three clients compute a different root.

The CI keystone job `cross-client-genesis-root` (defined in `.github/workflows/ci.yml`, exercising `examples/full-matrix-spec-feature.yaml`) re-asserts the invariant on every PR. When a divergence appears, the most likely cause is calibration drift (`internal/sizecal/`) or a missing syscontract preamble; less common but possible is per-client codec drift (`internal/reth/`, `internal/neth/`, etc.).

## Performance characteristics

State Actor's writer pipelines are streaming: total RAM stays bounded
(~2 GB peak for the spec-driven path, regardless of total state size)
because the streaming trie hasher (`internal/streamingtrie`) consumes one
account at a time over a sorted key stream (`internal/streamsort`),
emitting trie nodes as the stream advances rather than building the
trie in memory. End-to-end throughput is dominated by the chosen
client's on-disk format — Pebble compaction (geth), MDBX random-write
IOPS (reth), RocksDB compaction (besu / nethermind). Numbers vary by
host and would rot fast; benchmark on your own hardware with
`--benchmark --verbose` if you need a concrete figure.
