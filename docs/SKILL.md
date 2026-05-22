# SKILL.md — how to use state-actor

This is the canonical "how to use this lib" doc for agents. The root [`AGENTS.md`](../AGENTS.md) is a pointer that points here.

state-actor generates client-ready Ethereum databases for geth, reth, besu, and nethermind without going through each client's `init` path. You point it at a `--db` path, choose a `--client`, optionally pass a `--spec` declaring concrete entities, and it writes a database the client can boot against directly.

The repository has three deep references that this doc threads together:
- [`SPEC.md`](SPEC.md) — the `--spec` YAML schema.
- [`RUNBOOK.md`](RUNBOOK.md) — per-client boot recipes (geth / reth / besu / nethermind).
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — internal architecture; cross-client determinism; per-client writer differences.

## Three load-bearing flags

| Flag | What it controls |
|---|---|
| `--client` | Which client format to write (`geth`, `reth`, `besu`, `nethermind`) |
| `--spec` | Path to a YAML file declaring concrete entities (EOAs, contracts, ERC-20s, EIP-7702 delegations) |
| `--target-size` | Upper bound on the projected trie footprint of the whole DB |

Everything else has a sane default. Run `state-actor --help` for the full list (22 flags).

## Common tasks

### Generate a geth DB

```bash
go run . --db=/tmp/sa-geth/geth/chaindata --client=geth --target-size=100MB
```

Defaults: 1000 EOAs + 100 contracts of random storage, deterministic seed. Output is a Pebble database geth can boot with `--db.engine=pebble`. The `/geth/chaindata` suffix is mandatory — geth appends it to `--datadir`.

See [`RUNBOOK.md#geth`](RUNBOOK.md#geth) for the boot command, and [`client/geth/doc.go`](../client/geth/doc.go) for the on-disk layout and writer details.

### Generate a DB for a specific client

```bash
go run . --db=/tmp/sa-reth --client=reth --target-size=100MB        # MDBX + RocksDB + static files
go run . --db=/tmp/sa-besu --client=besu --target-size=100MB        # single RocksDB, 8 Bonsai CFs
go run . --db=/tmp/sa-neth --client=nethermind --target-size=100MB  # 7 RocksDB instances
```

`besu`, `nethermind`, and `reth` require cgo (RocksDB / MDBX bindings). On macOS, build via Docker — the repo ships per-client Dockerfiles. The per-client on-disk layout and pinned upstream version live in [`client/reth/doc.go`](../client/reth/doc.go), [`client/besu/doc.go`](../client/besu/doc.go), [`client/nethermind/doc.go`](../client/nethermind/doc.go).

### Write a spec — the canonical fixture is the syntax reference

[`examples/full-matrix-spec-feature.yaml`](../examples/full-matrix-spec-feature.yaml) is the CI-verified, 22-entity fixture that exercises every spec feature. Treat it as the source of truth for syntax. Mapping from intent to entity:

| To declare … | Look at fixture entity # | Notes |
|---|---|---|
| Plain EOA at explicit address | 1, 16 | `kind: eoa`, `address:`, `balance:`, `nonce:` |
| Plain EOA, name-derived address | 17 | `kind: eoa`, `name:`, no `address:` |
| Plain EOA, position-derived address | 18 | neither `name:` nor `address:` |
| Zero-balance / hex-form balance EOA | 20, 21 | `balance: "0"` / `balance: "0x..."` |
| Plain EOA with synthesized storage | 19 | `approximate_size_bytes:` on an EOA |
| EIP-7702 delegating EOA | 13, 14, 15 | `code: "0xef0100" + 20 bytes` (40 hex chars after `ef0100`) |
| EIP-7702 EOA + storage bloat | 14 | the `bloated-validator` entity |
| ERC-20 at explicit address with bulk fill | 2 | `template: erc20`, `parameters.total_owners` |
| ERC-20 name-derived | 3 | `template: erc20`, `name:`, no `address:` |
| ERC-20 position-derived | 4 | neither `name:` nor `address:` |
| ERC-20 skeleton (no holders) | 5 | omit `total_owners` and `owners:` |
| ERC-20 with `approximate_size_bytes` | 7 | `template + bloat` combo |
| ERC-20 with explicit `owners` and `allowances` | 8 | each is a list of `{address, balance}` / `{owner, spender, allowance}` |
| ERC-20 explicit + bulk combined | 9 | `total_owners` + `owners:` coexist; same for `total_allowances` + `allowances:` |
| Raw bytecode contract | 10, 11, 12 | `kind: contract`, `code:` (no `template:`) |

For the YAML schema reference (parser rules, validation errors, address-resolution algorithm, `approximate_size_bytes` semantics), read [`SPEC.md`](SPEC.md). For the package responsible for parsing and validation, see [`internal/spec/doc.go`](../internal/spec/doc.go); for resolution of derived addresses, [`internal/specbuild/doc.go`](../internal/specbuild/doc.go); for the template registry, [`internal/templates/doc.go`](../internal/templates/doc.go).

### Boot a generated DB on a client

Recipes (state-actor invocation + docker boot command + verification) live in [`RUNBOOK.md`](RUNBOOK.md), one section per client.

### Verify a generated DB

**Recommended**: run the per-client end-to-end suite. The Go oracle exercises `CheckChainID`, `CheckCanonicalSyscontracts`, `CheckChainAdvanced`, `CheckBeaconRootsRingBuffer`, and `CheckInjections` (see [`internal/e2e_testing/`](../internal/e2e_testing/)):

```bash
go test ./client/geth/...        # or ./client/reth/... ./client/besu/... ./client/nethermind/...
```

**Manual**: boot the client per [`RUNBOOK.md`](RUNBOOK.md), then:

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

The cross-client genesis-root invariant says: same `--seed`, same spec, same client-policy → identical state root across all four MPT clients. If it diverges, see [`ARCHITECTURE.md#cross-client-determinism`](ARCHITECTURE.md#cross-client-determinism) for the full check-list (per-client calibration in [`internal/sizecal/doc.go`](../internal/sizecal/doc.go); canonical syscontract preamble; CI keystone job).

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

The entry points cluster by the kind of extension:

| Extension | Read first | Pattern |
|---|---|---|
| New spec template | [`internal/templates/doc.go`](../internal/templates/doc.go) | Implement `Template` in a new file in `internal/templates/`; `Register(yourTemplate)` in `init()`; add a section to [`SPEC.md`](SPEC.md). Mirror `erc20.go`'s shape. |
| Change how spec entities resolve | [`internal/specbuild/doc.go`](../internal/specbuild/doc.go) | This is the seam between the YAML schema (parsed by `internal/spec`) and the writer-facing `[]PreAllocEntity` slice. Address modes live in `derive.go`. |
| Touch the YAML schema | [`internal/spec/doc.go`](../internal/spec/doc.go) | Pure data + parse + schema-time validation. Keep import-cycle-free with `internal/templates`. |
| Touch sizing / calibration | [`internal/sizecal/doc.go`](../internal/sizecal/doc.go) | One global `bytesPerSlot` constant per client; the CI invariance gate uses `NewFixed(64)` so test sizing can't mask a `Default()` drift. |
| Add or modify a client target | [`client/geth/doc.go`](../client/geth/doc.go) (template), [`client/reth/doc.go`](../client/reth/doc.go), [`client/besu/doc.go`](../client/besu/doc.go), [`client/nethermind/doc.go`](../client/nethermind/doc.go) | Add `client/<name>/` with a `Run` (or `RunCgo`) entry point, a `doc.go` documenting on-disk layout + pinned upstream version, and a `TestE2ESuite` driving Docker boot + the shared `e2e.RunSuitePhases` oracle. Wire into `internal/clientpolicy/policy.go`. |
| Work on the reth codec | [`internal/reth/doc.go`](../internal/reth/doc.go) | Byte-exact mirror of reth's MDBX schema + Compact codec. Pinned to a specific reth commit; updating requires regenerating `testdata/fixtures.json`. |
| Add a CLI flag | `main.go` + [`ARCHITECTURE.md`](ARCHITECTURE.md) | Declare in `main.go`, document in `--help` text, refresh the compact flag table in [`README.md`](../README.md). |

## Spec recipe builder

To build a custom spec, copy the relevant entity blocks from `examples/full-matrix-spec-feature.yaml` and adapt:

1. **Choose an address mode.** Explicit (set `address:`) — stable across runs and across spec reorderings. Name-derived (set `name:`, omit `address:`) — stable across runs but not reorderings; the address is `keccak256(seed || name)[12:]`. Position-derived (omit both) — depends on the entity's index in the YAML.

2. **Choose a kind.** `kind: eoa` or `kind: contract`. EOAs may carry an EIP-7702 delegation `code:` (the 23-byte `0xef0100<delegate-addr>` form). Contracts must set exactly one of `template:` or `code:`.

3. **Pick the right template if applicable.** Only `erc20` ships today. Its parameters: required `symbol`, `name`, `decimals` (must equal 18); optional `owners` (explicit holder list), `total_owners` (bulk fill), `allowances` (explicit), `total_allowances` (bulk). Schema details: [`SPEC.md`](SPEC.md).

4. **Set `approximate_size_bytes`** if you want synthetic storage on top of the template / raw code. Resolved to a slot count via per-client calibration in `internal/sizecal/factors.json`; accuracy is ±25 %.

5. **Generate**: `state-actor --client=<X> --db=<path> --spec=<your.yaml> --accounts=0 --contracts=0`. Pair with `--target-size` only when you want to cap the whole DB (spec entities count toward the cap and may be truncated).

6. **Verify the entities landed** at the addresses you expect: `cast code 0x<derived-address>` should return the bytecode (for contracts) or `0x` plus delegation marker (for 7702 EOAs).
