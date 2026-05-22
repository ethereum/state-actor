# State Actor

<p align="center">
  <img src="docs/logo.svg" alt="State Actor" width="200"/>
</p>

<p align="center">
  <strong>Generate client-ready Ethereum databases for geth, reth, besu, and nethermind &mdash; without going through each client's <code>init</code> path.</strong>
</p>

<p align="center">
  <a href="#quick-start">Quick Start</a> •
  <a href="#usage">Usage</a> •
  <a href="docs/RUNBOOK.md">Runbook</a> •
  <a href="docs/SPEC.md">Spec format</a> •
  <a href="docs/ARCHITECTURE.md">Architecture</a> •
  <a href="AGENTS.md">For agents</a>
</p>

> [!TIP]
> If you want Claude, Codex, Cursor, or another coding agent to operate
> state-actor for you, point it at [`AGENTS.md`](AGENTS.md) (the entry
> pointer) or directly at [`docs/SKILL.md`](docs/SKILL.md) (the deep doc).
> The canonical syntax reference for what `--spec` can express is
> [`examples/full-matrix-spec-feature.yaml`](examples/full-matrix-spec-feature.yaml) —
> CI keeps it correct.

---

## Why

You want a pre-populated Ethereum database that a client can boot against directly &mdash; for cross-client determinism tests, devnet bootstrapping, EIP-7702 / ERC-20 fixtures, or state-bloat experiments. The alternative (run the client's `init` against a genesis with millions of `alloc` entries) is slow and client-specific. State Actor writes each client's on-disk format directly: Pebble for geth, MDBX + RocksDB + nippy-jar for reth, single RocksDB + 8 Bonsai column families for besu, seven RocksDB instances for nethermind.

Three flags carry most of the weight: `--client` (which client's format to write), `--spec` (concrete entities to include, declared in YAML), `--target-size` (upper bound on the whole DB; truncates spec entities to fit, stops synthetic fill at the cap). Everything else has a sane default.

## Quick start

```bash
# Smoke test: a small geth DB, defaults, no spec.
go run . --client=geth --db=/tmp/sa-geth/geth/chaindata --target-size=100MB

# Spec-driven: load a curated YAML verbatim (no --target-size, so the
# spec is never truncated; --accounts=0 --contracts=0 suppresses
# synthetic fill).
go run . --client=geth --db=/tmp/sa-spec/geth/chaindata \
  --spec=examples/spec-minimal.yaml \
  --accounts=0 --contracts=0

# Pick a different client (Docker required for cgo clients).
go run . --client=reth --db=/tmp/sa-reth \
  --spec=examples/spec-minimal.yaml --accounts=0 --contracts=0
```

After the run, boot the client against the produced datadir &mdash; the per-client recipes are in [`docs/RUNBOOK.md`](docs/RUNBOOK.md).

## Installation

```bash
git clone https://github.com/nerolation/state-actor.git
cd state-actor
go build -o state-actor .            # geth client only (pure Go)
docker build -f Dockerfile.reth -t state-actor-reth .  # cgo clients
```

`besu`, `nethermind`, and `reth` need cgo bindings (RocksDB / MDBX). On macOS, build them via Docker; per-client `Dockerfile.<client>` files ship in this repo.

## Usage

### Generate a geth database

Geth has a pure-Go writer. The path you pass to `--db` must end in `/geth/chaindata` &mdash; geth itself appends that suffix to its `--datadir`.

```bash
state-actor --client=geth --db=/tmp/sa-geth/geth/chaindata --target-size=1GB
```

### Generate for reth, besu, or nethermind

```bash
docker run --rm -v /tmp/sa-reth:/data state-actor-reth \
  ./state-actor --client=reth --db=/data --target-size=1GB
```

Substitute `besu` / `nethermind` for `reth` (and pick the matching Dockerfile). The on-disk layout is documented per client in [`docs/RUNBOOK.md`](docs/RUNBOOK.md).

### Declare concrete entities with a spec

The `--spec` flag points at a YAML file describing exactly which contracts and EOAs to write &mdash; ERC-20 tokens with chosen sizes, EIP-7702 delegating EOAs, raw bytecode contracts, address-mode demonstrations. See [`docs/SPEC.md`](docs/SPEC.md) for the schema; [`examples/README.md`](examples/README.md) is the picker.

```bash
state-actor --client=geth --db=/tmp/sa/geth/chaindata \
  --spec=examples/spec-erc20-mixed-sizes.yaml \
  --accounts=0 --contracts=0
```

When you use `--spec`, set `--accounts=0 --contracts=0` to suppress synthetic fill &mdash; otherwise random EOAs and contracts can collide with spec-derived addresses.

### Cap the database size

`--target-size` is an upper bound on the projected trie footprint of the whole generated database. Both spec entities and the synthetic-fill loop count toward the budget; if the spec alone would exceed the budget, the spec is silently truncated to the longest prefix that fits (with a warning on stderr). To generate a spec verbatim, omit `--target-size`.

```bash
state-actor --client=reth --db=/tmp/sa --target-size=10GB
```

Accepted suffixes: `KB`, `MB`, `GB`, `TB` (base-1024). Bare numbers are bytes.

### Tune the genesis chainspec

```bash
state-actor --client=geth --db=/tmp/sa/geth/chaindata \
  --chain-id=12345 \
  --fork=osaka \
  --gas-limit=60000000 \
  --timestamp=1700000000 \
  --extra-data=0xdeadbeef
```

Run `state-actor --list-forks` for accepted `--fork` values. The default fork is the latest one each `--client` supports (currently `osaka` across all four).

## Boot a client against the generated DB

The boot command differs per client. The full recipes &mdash; verbatim from each client's `TestE2ESuite` &mdash; are in [`docs/RUNBOOK.md`](docs/RUNBOOK.md).

| Client | Recipe |
|---|---|
| geth | [docs/RUNBOOK.md#geth](docs/RUNBOOK.md#geth) |
| reth | [docs/RUNBOOK.md#reth](docs/RUNBOOK.md#reth) |
| besu | [docs/RUNBOOK.md#besu](docs/RUNBOOK.md#besu) |
| nethermind | [docs/RUNBOOK.md#nethermind](docs/RUNBOOK.md#nethermind) |

## Spec system at a glance

A spec file lists entities &mdash; EOAs or contracts &mdash; with explicit addresses, name-derived addresses (`keccak256(seed || name)[12:]`), or position-derived addresses. Contracts can use a template (`erc20` is shipped today) or carry raw bytecode. EIP-7702 delegating EOAs are first-class.

```yaml
entities:
  - kind: eoa
    address: 0x1111111111111111111111111111111111111111
    balance: "1000000000000000000"

  - kind: contract
    name: usdc-mock                           # name-derived address
    template: erc20
    parameters: { symbol: USDC, name: USD Coin, decimals: 18, total_owners: 1000 }
    approximate_size_bytes: 100000000          # ~100 MB synthetic storage
```

Full schema: [`docs/SPEC.md`](docs/SPEC.md). Curated examples: [`examples/README.md`](examples/README.md).

## CLI flags

| Flag | Type | Default | Purpose |
|---|---|---|---|
| `--db` | string | (required) | Path to the database directory |
| `--client` | string | `geth` | Target client: `geth`, `nethermind`, `besu`, `reth` |
| `--spec` | string | (none) | Path to a YAML state-spec file; see `docs/SPEC.md` |
| `--target-size` | string | (none) | Upper bound on the whole DB (`5GB`, `500MB`, …). Truncates spec to the longest prefix that fits; synthetic fill stops at the cap. |
| `--accounts` | int | 1000 | Number of synthetic EOAs |
| `--contracts` | int | 100 | Number of synthetic contracts |
| `--min-slots` | int | 1 | Min storage slots per synthetic contract |
| `--max-slots` | int | 10000 | Max storage slots per synthetic contract |
| `--distribution` | string | `power-law` | Synthetic storage distribution: `power-law`, `uniform`, `exponential` |
| `--code-size` | int | 1024 | Average synthetic contract code size, bytes |
| `--seed` | int | 1 | Random seed; `--seed=0` randomises (footgun) |
| `--fork` | string | (latest) | Hard fork active at genesis; `--list-forks` lists choices |
| `--chain-id` | int | 1337 | Chain ID embedded in the synthesized chainspec |
| `--gas-limit` | uint | 30000000 | Genesis block gas limit |
| `--timestamp` | uint | 0 | Genesis block timestamp (unix seconds) |
| `--extra-data` | string | (empty) | Genesis block extraData (hex) |
| `--archive` | bool | false | Archive-mode metadata; geth + reth only |
| `--binary-trie` | bool | false | EIP-7864 binary trie; geth only |
| `--group-depth` | int | 8 | Binary-trie group depth (1-8) |
| `--list-forks` | bool | false | Print accepted `--fork` values and exit |
| `--verbose` | bool | false | Verbose output |
| `--benchmark` | bool | false | Print detailed timing stats |

Run `state-actor --help` for the canonical list (this table is a snapshot).

## When NOT to use State Actor

- You need a real testnet's history. State Actor writes one genesis block; there is no chain to replay.
- You need post-genesis transactions in the DB. The output is genesis state; drive the chain forward with a separate tool (`spamoor`, your own test harness).
- The exact byte-shape of mainnet trie nodes matters more than state size. State Actor synthesises shapes; it does not mirror live state.

## Architecture

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the deep dive (writer phases, per-client format notes, the streaming-trie / streaming-sort packages). The short version: entity generation streams into a per-client writer that produces the client-native database directly, with cross-client determinism guaranteed by `internal/sizecal/`'s single global `bytesPerSlot` constant (identical across all four clients by design).

## Testing

```bash
go test ./...                               # full suite
go test -run TestE2ESuite ./client/...      # per-client end-to-end (Docker required for cgo clients)
go test -short ./...                        # skip the e2e suites
```

CI matrix lives in `.github/workflows/ci.yml`. The cross-client `cross-client-genesis-root` job pins the invariant: same `--seed` + same spec → identical state root across all four MPT clients.

## Contributing

Read [`CONTRIBUTING.md`](CONTRIBUTING.md). Agent collaborators should start at [`AGENTS.md`](AGENTS.md).

## License

MIT &mdash; see [`LICENSE`](LICENSE).

## Acknowledgments

- [go-ethereum](https://github.com/ethereum/go-ethereum) for the database and state primitives.
- [reth](https://github.com/paradigmxyz/reth) for the MDBX writer reference.
- [hyperledger/besu](https://github.com/hyperledger/besu) and [nethermind](https://github.com/NethermindEth/nethermind) for the chainspec formats.
- [ethereum-package](https://github.com/ethpandaops/ethereum-package) for Kurtosis integration patterns.
