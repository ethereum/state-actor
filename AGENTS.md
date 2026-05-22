# AGENTS.md

state-actor generates client-ready Ethereum databases for geth, reth, besu, and nethermind without going through each client's `init` path. You point it at a `--db` path, choose a `--client`, optionally pass a `--spec` declaring concrete entities, and it writes a database the client can boot against directly.

## Orient yourself

| Topic | Where |
|---|---|
| Spec YAML format | [`docs/SPEC.md`](docs/SPEC.md) |
| Client boot recipes | [`docs/RUNBOOK.md`](docs/RUNBOOK.md) |
| Internal architecture | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| Examples (with picker) | [`examples/README.md`](examples/README.md) |
| Full CLI flag list | `state-actor --help` |

Three flags carry most of the weight: `--client` (which database to write), `--spec` (what concrete state to include), `--target-size` (how big to make it). Everything else has a sane default.

## Common tasks

### Generate a geth DB

```bash
go run . --db=/tmp/sa-geth --client=geth --target-size=100MB
```

Defaults: 1000 EOAs + 100 contracts of random storage, deterministic seed. Output is a Pebble database geth can boot with `--db.engine=pebble`. See [`docs/RUNBOOK.md#geth`](docs/RUNBOOK.md#geth).

### Generate a DB for a specific client

```bash
go run . --db=/tmp/sa-reth --client=reth --target-size=100MB        # MDBX + RocksDB + static files
go run . --db=/tmp/sa-besu --client=besu --target-size=100MB        # single RocksDB, 8 Bonsai CFs
go run . --db=/tmp/sa-neth --client=nethermind --target-size=100MB  # 7 RocksDB instances
```

`besu`, `nethermind`, and `reth` require cgo (RocksDB / MDBX bindings). On macOS, build via Docker — the repo ships per-client Dockerfiles.

### Add an ERC-20 with a chosen storage size

`approximate_size_bytes` tells the spec builder how much synthetic storage to attach to a template instance — clients see a token whose `_balances` mapping has that many slots populated.

```yaml
entities:
  - kind: contract
    name: usdc-mock
    template: erc20
    parameters:
      symbol: USDC
      name: USD Coin
      decimals: 18
      total_owners: 1000
    approximate_size_bytes: 100000000  # ~100 MB of synthetic storage
```

Full template parameters (incl. `owners`, `allowances`, `total_allowances`) are documented in [`docs/SPEC.md`](docs/SPEC.md).

### Add an EIP-7702 delegating EOA

```yaml
entities:
  - kind: eoa
    name: bloated-7702
    balance: "1000000000000000000"
    code: "0xef0100" + "<20-byte delegate>"  # EIP-7702 delegation marker
    approximate_size_bytes: 50000000        # synthesized storage on the delegate
```

The delegation marker `0xef0100` is a magic prefix the spec system recognises; state-actor injects synthesized storage at the delegate.

### Boot a generated DB on a client

Recipes (state-actor invocation + docker boot command + verification) live in [`docs/RUNBOOK.md`](docs/RUNBOOK.md), one section per client.

### Verify a generated DB

**Recommended**: run the per-client end-to-end suite, which exercises `CheckChainID`, `CheckCanonicalSyscontracts`, `CheckChainAdvanced`, `CheckBeaconRootsRingBuffer`, and `CheckInjections` (see [`internal/e2e_testing/`](internal/e2e_testing/)):

```bash
go test ./client/geth/...        # or ./client/reth/... ./client/besu/... ./client/nethermind/...
```

**Manual**: boot the client per [`docs/RUNBOOK.md`](docs/RUNBOOK.md), then:

```bash
cast chain-id --rpc-url http://localhost:8545     # → 0x539 (1337)
cast balance 0x<a-known-spec-address> --rpc-url ... # → the spec's balance
cast code    0x<a-known-spec-contract> --rpc-url ... # → the spec's bytecode
```

### Reproduce a CI failure locally

CI uses `examples/full-matrix-spec-feature.yaml`, `--seed=42`, `--accounts=0`, `--contracts=0` against each client's `TestE2ESuite`. Re-run:

```bash
go test -run TestE2ESuite ./client/<client>/... -v
```

### Diagnose "wrong state root"

The cross-client genesis-root invariant says: same `--seed`, same spec, same client-policy → identical state root across all four MPT clients. If it diverges, the most likely cause is a per-client calibration drift (see `internal/sizecal/`) or a missing canonical syscontract. [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#cross-client-determinism) has the full check-list.

## Constraints & gotchas

- **Besu / reth / nethermind require Docker** for the writer side (cgo dependencies; no native build on macOS). Only geth has a pure-Go writer.
- **`--seed=0` is a footgun**: `main.go` rewrites it to `time.Now().UnixNano()`, i.e. randomises. For determinism use any non-zero seed. Bench convention is `--seed=42`.
- **`--target-size` is an upper bound on the whole database**: spec entities AND synthetic fill both count toward it. If the spec alone exceeds the budget, `internal/specbuild` silently truncates the entity list to the longest prefix that fits and emits a warning. To generate a spec verbatim without truncation risk, omit `--target-size`.
- **`--archive` is geth/reth only**: rejected for besu and nethermind.
- **`--binary-trie` is geth-only** (EIP-7864).
- **When using `--spec`, pair it with `--accounts=0 --contracts=0`** to suppress synthetic fill — otherwise random EOAs and contracts can collide with spec-derived addresses.
- **Nethermind boot needs a JSON `boot.cfg`** pointing at the chainspec + datadir (see `client/nethermind/e2e_test.go`'s `nethermindE2EConfigTemplate`). The other clients accept boot flags directly.

## Testing

```bash
go test ./...                          # full suite
go test -run TestE2ESuite ./client/... # per-client end-to-end
go test -short ./...                   # skip the e2e suites
```

CI matrix lives in `.github/workflows/ci.yml`.

## When asked to extend state-actor

- **New spec template** → `internal/templates/` (mirror `erc20.go`'s shape; register in `registry.go`; add an entry to `UserVisibleNames()` and a section to `docs/SPEC.md`).
- **New client target** → add `client/<name>/` mirroring `client/geth/`'s shape: a `Run` (or `RunCgo`) entry point, a `doc.go` documenting on-disk layout, a `TestE2ESuite` that drives docker boot + the shared `e2e.RunSuitePhases` oracle. Wire into `internal/clientpolicy/policy.go`.
- **New CLI flag** → declare in `main.go`, document in `state-actor --help` text, refresh the compact flag table in `README.md`.
