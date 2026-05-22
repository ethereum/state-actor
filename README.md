# State Actor

<p align="center">
  <img src="docs/logo.svg" alt="State Actor" width="200"/>
</p>

<p align="center">
  <strong>High-performance Ethereum state generator for devnet testing</strong>
</p>

<p align="center">
  <a href="#quick-start">Quick Start</a> •
  <a href="#features">Features</a> •
  <a href="#usage">Usage</a> •
  <a href="#integration">Integration</a> •
  <a href="docs/ARCHITECTURE.md">Architecture</a>
</p>

---

State Actor generates realistic Ethereum state directly into a geth-compatible Pebble database (snapshot layer). Create bloated devnets with millions of accounts and storage slots to test client behavior under mainnet-like conditions.

## Quick Start

```bash
# Install
go install github.com/nerolation/state-actor@latest

# Generate
state-actor \
    --db ./chaindata \
    --target-size 100MB \
    --seed 42

# Output:
# State Root:  0x8e170135992c...
# Genesis:     included (ready to use without geth init)
```

**No `geth init` required** — the database is ready to use immediately.

## Features

| Feature | Description |
|---------|-------------|
| ⚡ **Fast** | 350K+ storage slots/second |
| 🎯 **Realistic** | Auto-fill emits a mainnet-shaped 20 % account-trie / 10 % bytecode / 70 % storage split |
| 🔄 **Reproducible** | Seed-based generation for consistent tests |
| 🔗 **Genesis Integration** | Merges with genesis.json, writes genesis block |
| 📦 **Ready to Use** | No `geth init` needed — produces a geth-compatible Pebble database |
| 🐳 **Docker Ready** | Pre-built images available |

## Installation

### From Source

```bash
git clone https://github.com/nerolation/state-actor.git
cd state-actor
go build -o state-actor .
```

### Using Go Install

```bash
go install github.com/nerolation/state-actor@latest
```

### Docker

```bash
docker pull ghcr.io/nerolation/state-actor:latest
# or build locally
docker build -t state-actor:latest .
```

## Usage

### Basic Usage

```bash
# Minimal: auto-fill 100 MB of mainnet-shaped state
state-actor --db ./chaindata --target-size 100MB

# With a declarative spec (overlays auto-fill on top of the spec entities)
state-actor \
    --db ./chaindata \
    --spec ./examples/full-matrix-spec-feature.yaml \
    --target-size 1GB
```

`--target-size` is **required** unless `--spec` is set. The auto-fill emits
synthetic state in mainnet-shaped proportions (20 % account-trie / 10 %
bytecode / 70 % storage); per-contract code is a truncated normal in
`[1 KiB, 24 KiB]` centered at 5 KiB, and per-contract storage size is a
truncated normal in `[1 KiB, 100 MiB]` whose mean is budget-derived.
EOAs randomize balance / nonce / EIP-7702 delegation independently.

### Command Line Options

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | (required) | Output database directory |
| `--target-size` | - | Required unless `--spec` is set. Target DB size (e.g. `5GB`, `500MB`). Drives auto-fill of 20/10/70 mainnet-shaped synthetic state. With `--spec`, fills the headroom after the spec's projected cost. |
| `--spec` | - | YAML state-spec file (see `docs/SPEC.md`). |
| `--seed` | 1 | Random seed; pass `--seed=0` to use wall-clock time (non-reproducible). |
| `--binary-trie` | false | Generate state for EIP-7864 binary trie mode |
| `--chain-id` | 1337 | Chain ID embedded in the synthesized chainspec |
| `--client` | geth | Target Ethereum client: `geth`, `nethermind`, `besu`, or `reth` |
| `--archive` | false | Generate archive-mode DB (geth + reth only) |
| `--verbose` | false | Verbose output |
| `--benchmark` | false | Print detailed stats |

### Output Format

State Actor produces a Pebble database with geth's snapshot layer format.
Ready to use with `geth --db.engine=pebble`.

```bash
state-actor --db ./chaindata --genesis genesis.json ...
```

| Aspect | Value |
|--------|-------|
| Database | Pebble |
| Account key | `a` + keccak(addr) |
| Storage key | `o` + keccak(addr) + keccak(slot) |
| Encoding | SlimAccountRLP |

### Trie Modes

By default, State Actor uses the **Merkle Patricia Trie (MPT)** for state root computation, matching standard Ethereum. To generate state for **binary trie mode** (EIP-7864), pass `--binary-trie`:

```bash
state-actor --db ./chaindata --target-size 100MB --binary-trie
```

Binary trie state requires geth to run with `--override.verkle=0` (legacy flag name for EIP-7864).

> **Important:** Binary trie mode requires geth built from the same `go-ethereum` version
> referenced in this project's `go.mod` (the binary trie key derivation must match). Using a
> different geth version may produce incompatible state that geth cannot read.

### Recommended Configurations

#### Local Testing (Quick)
```bash
state-actor --db ./chaindata --target-size 10MB --seed 1
```

#### CI/CD Pipeline
```bash
state-actor --db ./chaindata --target-size 100MB --seed 42
```

#### Mainnet-like State
```bash
state-actor --db ./chaindata --target-size 100GB --seed 12345
```

The auto-fill emits a fixed 20 / 10 / 70 split (account-trie / bytecode /
storage) regardless of target size. The 24 KiB per-contract code clamp is
mainnet-accurate (EIP-170); storage size per contract is a truncated
normal in `[1 KiB, 100 MiB]` whose mean adapts to the target budget.

#### With a Spec
```bash
state-actor \
    --db ./chaindata \
    --spec ./examples/full-matrix-spec-feature.yaml \
    --target-size 1GB
```

The spec entities materialize as declared; `--target-size` fills the
headroom with auto-filled synthetic state. If the spec already meets
or exceeds the target, no auto-fill runs.

## Genesis Integration

When `--genesis` is provided, State Actor:

1. **Loads genesis.json** — parses chain config and alloc accounts
2. **Merges accounts** — includes alloc accounts at their exact addresses
3. **Generates state** — adds random accounts/contracts
4. **Computes state root** — combined root via StackTrie
5. **Writes genesis block** — with correct state root

This eliminates the state root mismatch problem and removes the need for `geth init`.

### Supported Genesis Format

Standard geth genesis.json:

```json
{
  "config": {
    "chainId": 32382,
    "shanghaiTime": 0,
    "cancunTime": 0,
    "terminalTotalDifficulty": 0
  },
  "gasLimit": "0x1c9c380",
  "difficulty": "0x0",
  "alloc": {
    "0x123...": { "balance": "0x..." },
    "0xabc...": { "code": "0x...", "storage": {...} }
  }
}
```

## Integration with Kurtosis / ethereum-package

See [docs/KURTOSIS.md](docs/KURTOSIS.md) for detailed integration guide.

### Quick Integration

```bash
# 1. Generate state
state-actor --db ./chaindata --target-size 1GB --seed 42

# 2. Copy to geth data directory
mkdir -p ./geth-data/geth
cp -r ./chaindata ./geth-data/geth/chaindata

# 3. Start geth (no init needed)
geth --datadir ./geth-data --db.engine=pebble ...
```

## Auto-Fill Distribution

The auto-fill emits synthetic state in a fixed mainnet-shaped split:

| Category | Share | Per-entity shape |
|----------|------:|------------------|
| Account trie | 20 % | 175 B per leaf — EOAs (90 % non-zero balance, 30 % EIP-7702 delegation, always non-zero nonce) and contract headers |
| Bytecode | 10 % | Truncated normal centered at 5 KiB, clamped to `[1 KiB, 24 KiB]` (EIP-170-compliant) |
| Storage | 70 % | Truncated normal in `[1 KiB, 100 MiB]` with budget-derived mean |

Contract count is pinned by the bytecode budget so per-contract code stays
mainnet-realistic; EOA count is derived from the remaining account-trie
budget. The ratios apply to the top-up portion only — `target_size` minus
the projected cost of any `--spec` entities.

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for detailed architecture documentation.

```
┌─────────────────┐     ┌─────────────────┐
│   CLI (main.go) │────▶│  genesis.json   │
└────────┬────────┘     └────────┬────────┘
         │                       │
         ▼                       ▼
┌─────────────────────────────────────────┐
│              Generator                  │
│  • Genesis accounts + Generated state   │
│  • StackTrie (MPT) or BinaryTrie root   │
└────────────────────┬────────────────────┘
                     ▼
           ┌─────────────────┐
           │   GethWriter    │
           │   (Pebble)      │
           │   Snapshot fmt  │
           └─────────────────┘
```

## Database Schema

Snapshot layer format (Pebble):

| Key | Value |
|-----|-------|
| `a` + keccak(addr) | SlimAccountRLP |
| `o` + keccak(addr) + keccak(slot) | RLP(value) |
| `c` + keccak(code) | bytecode |
| `SnapshotRoot` | state root |

Plus genesis metadata when `--genesis` is provided.

## Testing

```bash
# Run all tests
go test -v ./...

# With race detector
go test -race ./...

# Run benchmarks
go test -bench=. ./generator
```

## Contributing

Contributions welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) first.

## License

MIT License - see [LICENSE](LICENSE)

## Acknowledgments

- [go-ethereum](https://github.com/ethereum/go-ethereum) for the database and state primitives
- [ethereum-package](https://github.com/ethpandaops/ethereum-package) for Kurtosis integration patterns
