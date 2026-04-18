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
│  │  • Load genesis.json (optional)                                     │ │
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

```go
// Load genesis (optional)
genesis, _ := genesis.LoadGenesis("genesis.json")

// Create config with genesis accounts
config := generator.Config{
    GenesisAccounts: genesis.ToStateAccounts(),
    GenesisStorage:  genesis.GetAllocStorage(),
    GenesisCode:     genesis.GetAllocCode(),
    // ... other config
}

// Create generator (opens Pebble DB)
gen, _ := generator.New(config)
```

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

## File Structure

```
state-actor/
├── main.go                    # CLI entry point
├── generator/
│   ├── config.go              # Configuration types
│   ├── generator.go           # Core generation logic
│   ├── deep_branch.go         # Deep-branch phantom key construction
│   ├── writer_geth.go         # Geth/Pebble snapshot writer
│   └── generator_test.go      # Unit tests
├── genesis/
│   ├── genesis.go             # Genesis loading and writing
│   └── genesis_test.go        # Unit tests
├── integration/
│   ├── stategen_launcher.star # Kurtosis integration
│   └── geth-wrapper.sh        # Wrapper script
├── examples/
│   └── test-genesis.json      # Example genesis file
└── docs/
    ├── ARCHITECTURE.md        # This file
    └── KURTOSIS.md            # Kurtosis integration guide
```

## Performance Characteristics

| Operation | Throughput | Bottleneck |
|-----------|------------|------------|
| Account generation | ~1M/s | CPU (hashing) |
| Storage generation | ~500K/s | Memory allocation |
| Batch writing | ~350K slots/s | Pebble compaction |
| State root computation | O(n log n) | StackTrie sorting |

The overall throughput is bounded by Pebble's write performance at ~350K storage slots/second.
