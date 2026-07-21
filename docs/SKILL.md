---
name: state-actor
description: Use this skill when a user wants to generate, boot, verify, or extend a state-actor-produced Ethereum database. Covers --client, --spec, --target-size, per-client boot recipes (geth / reth / besu / nethermind / ethrex / erigon), and the canonical 29-entity spec fixture.
---

# SKILL.md — how to use state-actor

This is the canonical "how to use this lib" doc for agents. The root [`AGENTS.md`](../AGENTS.md) is a pointer that points here.

state-actor generates client-ready Ethereum databases for geth, reth, besu, nethermind, ethrex, and erigon without going through each client's `init` path. You point it at a `--db` path, choose a `--client`, optionally pass a `--spec` declaring concrete entities, and it writes a database the client can boot against directly.

## Read these first

Read these in this order. The first one is load-bearing — read it before you read any prose about specs.

1. [`examples/full-matrix-spec-feature.yaml`](../examples/full-matrix-spec-feature.yaml) — **the canonical syntax reference for `--spec`**. CI-pinned, 29 entities, every feature. Read this file before reading any other doc about specs; see the [Canonical spec reference](#canonical-spec-reference) section below for an intent → entity-# index.
2. [`SPEC.md`](SPEC.md) — the schema reference (parser rules, validation errors, address-resolution algorithm, `approximate_size_bytes` semantics). Read alongside the fixture.
3. [`RUNBOOK.md`](RUNBOOK.md) — per-client boot recipes (geth / reth / besu / nethermind / ethrex / erigon).
4. [`ARCHITECTURE.md`](ARCHITECTURE.md) — internal architecture; cross-client determinism; per-client writer differences.

## Three load-bearing flags

| Flag | What it controls |
|---|---|
| `--client` | Which client format to write (`geth`, `reth`, `besu`, `nethermind`, `ethrex`, `erigon`) |
| `--spec` | Path to a YAML file declaring concrete entities (EOAs, contracts, ERC-20s, EIP-7702 delegations) |
| `--target-size` | Advisory budget that sizes the auto-fill of synthetic state. Not a hard on-disk cap — actual size may vary per client. |

Everything else has a sane default. Run `state-actor --help` for the full list (22 flags).

## Canonical spec reference

**The single source of truth for what a `--spec` YAML can express is [`examples/full-matrix-spec-feature.yaml`](../examples/full-matrix-spec-feature.yaml).** Read that file before reading anything else about specs. It is exhaustive (29 entities, every feature), self-documenting (six section banners + per-entity comments), and CI keeps it correct.

The CI guarantees that hold this file as the canonical reference:

- [`internal/specbuild/full_matrix_test.go`](../internal/specbuild/full_matrix_test.go) (`TestBuildFullMatrix`) pins the entity count at 29 and asserts the cross-client `PreAlloc` count equality. The unit-level drift gate — byte-identity is enforced downstream by the cross-client-genesis-root aggregator job.
- [`internal/e2e_testing/spec_setup.go`](../internal/e2e_testing/spec_setup.go) (`LoadCISpec`) loads the fixture with `seed=0` + `sizecal.NewFixed(64)` so every client sees the same input.
- Per-client `TestE2ESuite` (in [`client/geth/e2e_test.go`](../client/geth/e2e_test.go), [`client/besu/e2e_test.go`](../client/besu/e2e_test.go), [`client/nethermind/e2e_test.go`](../client/nethermind/e2e_test.go), [`client/reth/oracle_test.go`](../client/reth/oracle_test.go) for reth, [`client/ethrex/e2e_test.go`](../client/ethrex/e2e_test.go) for ethrex, and [`client/erigon/e2e_test.go`](../client/erigon/e2e_test.go) for erigon) boots a real client against state-actor's output, runs spamoor, and verifies every entity via the Go oracle.
- The `cross-client · genesis state-root invariant` aggregator job in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) refuses to merge a PR whose four clients produce different genesis roots from this fixture.

The fixture is organised into six labelled sections; each one pins a feature cluster:

| Section banner (in the YAML) | Entities | Feature pinned |
|---|---|---|
| `1. Spamoor sender` | 1 | Anvil dev-account-0 EOA with high balance |
| `2-7. ERC-20 flavor coverage` | 2-7 | All three address modes × bulk-fill × storage-bloat × nonce override × skeleton |
| `8-9. Explicit-owners + allowances coverage` | 8-9 | Granular `owners` list, explicit + bulk `allowances` |
| `10-12. Raw bytecode flavors` | 10-12 | `kind: contract` + `code:` (no template); large code; storage synth |
| `13-15. EIP-7702 EOA flavors` | 13-15 | `0xef0100<delegate>` delegation marker × address modes × storage bloat |
| `16-22. Plain EOA flavors` | 16-22 | All address modes, zero balance, hex-form balance, default-nonce, storage on EOA |

### Intent → entity-# index

| To declare … | Look at fixture entity # | Section |
|---|---|---|
| Plain EOA at explicit address | 1, 16 | Spamoor sender / Plain EOA flavors |
| Plain EOA, name-derived address | 17 | Plain EOA flavors |
| Plain EOA, position-derived address | 18 | Plain EOA flavors |
| Zero-balance or hex-form balance EOA | 20, 21 | Plain EOA flavors |
| Plain EOA with synthesized storage | 19 | Plain EOA flavors |
| EIP-7702 delegating EOA | 13, 14, 15 | EIP-7702 EOA flavors |
| EIP-7702 EOA + storage bloat | 14 (`bloated-validator`) | EIP-7702 EOA flavors |
| ERC-20 at explicit address with bulk fill | 2 | ERC-20 flavor coverage |
| ERC-20 name-derived | 3 | ERC-20 flavor coverage |
| ERC-20 position-derived | 4 | ERC-20 flavor coverage |
| ERC-20 skeleton (no holders) | 5 | ERC-20 flavor coverage |
| ERC-20 with explicit `nonce` override | 6 | ERC-20 flavor coverage |
| ERC-20 with `approximate_size_bytes` (template + bloat) | 7 | ERC-20 flavor coverage |
| ERC-20 with explicit `owners` and `allowances` | 8 | Explicit-owners + allowances |
| ERC-20 explicit + bulk combined | 9 | Explicit-owners + allowances |
| Raw bytecode contract | 10, 11, 12 | Raw bytecode flavors |
| Plain EOA with omitted-nonce default (0) | 22 | Plain EOA flavors |

### How to adapt the fixture to a new spec

1. **Find the entity that matches your intent** using the index above; jump to that entity in [`examples/full-matrix-spec-feature.yaml`](../examples/full-matrix-spec-feature.yaml).
2. **Copy the entity block** into your new YAML.
3. **Edit the load-bearing fields** for your case — addresses, balances, template parameters, `approximate_size_bytes`. The field-by-field cheatsheet in [`examples/README.md`](../examples/README.md#field-by-field-cheatsheet) lists exactly which fields control what and what the constraints are (e.g. `decimals` must be 18 for the `erc20` template).
4. **Address mode**: explicit (`address:` is set), name-derived (`name:` only), or position-derived (neither). The mode determines stability of the resulting address; see [`SPEC.md` § Address resolution](SPEC.md#address-resolution-three-deterministic-modes) for the algorithm.
5. **Omit `--target-size`** when running `--spec` alone — that way no auto-fill runs on top, eliminating the collision risk between random EOAs and spec-derived addresses. Set `--target-size` only when you intentionally want auto-fill to pad the headroom.
6. **Verify the entities landed** at the addresses you expect: `cast code 0x<derived-address>` returns the bytecode (contracts) or `0x` plus the delegation marker (7702 EOAs); `cast balance 0x<address>` returns the spec's balance.

For the schema-level details the fixture's per-entity comments don't cover — parser rules, validation errors, the address-derivation algorithm, `approximate_size_bytes` resolution semantics — read [`SPEC.md`](SPEC.md). It's the schema reference; the fixture is the syntax reference; you typically need both. Internal references: [`internal/spec/doc.go`](../internal/spec/doc.go) (parser + validator), [`internal/specbuild/doc.go`](../internal/specbuild/doc.go) (entity resolution), [`internal/templates/doc.go`](../internal/templates/doc.go) (template registry).

## Common tasks

### Generate a geth DB

```bash
go run . --db=/tmp/sa-geth/geth/chaindata --client=geth --target-size=100MB
```

Auto-fill emits mainnet-shaped state (20 % account-trie / 10 % bytecode / 70 % contract storage) up to the `--target-size` cap, with a deterministic seed. Output is a Pebble database geth can boot with `--db.engine=pebble`. The `/geth/chaindata` suffix is mandatory — geth appends it to `--datadir`.

See [`RUNBOOK.md#geth`](RUNBOOK.md#geth) for the boot command, and [`client/geth/doc.go`](../client/geth/doc.go) for the on-disk layout and writer details.

### Generate a DB for a specific client

```bash
go run . --db=/tmp/sa-reth --client=reth --target-size=100MB        # MDBX + RocksDB + static files
go run . --db=/tmp/sa-besu --client=besu --target-size=100MB        # single RocksDB, 8 Bonsai CFs
go run . --db=/tmp/sa-neth --client=nethermind --target-size=100MB  # 7 RocksDB + flat column DB
go run . --db=/tmp/sa-ethrex --client=ethrex --target-size=100MB   # single RocksDB, 20 CFs
go run . --db=/tmp/sa-erigon --client=erigon --target-size=100MB   # Erigon v3 flat .kv snapshots + minimal MDBX
```

`besu`, `nethermind`, `reth`, `ethrex`, and `erigon` require cgo (RocksDB / MDBX bindings). On macOS, build via Docker — the repo ships per-client Dockerfiles. The per-client on-disk layout and pinned upstream version live in [`client/reth/doc.go`](../client/reth/doc.go), [`client/besu/doc.go`](../client/besu/doc.go), [`client/nethermind/doc.go`](../client/nethermind/doc.go), [`client/ethrex/doc.go`](../client/ethrex/doc.go), [`client/erigon/doc.go`](../client/erigon/doc.go).

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

### Reproduce a run from its manifest

Every run drops a `state-actor-manifest.json` at the datadir root (two levels up from `--db` for geth; the `--db` dir itself for the other clients) recording everything needed to reproduce it: the resolved flags — note the **resolved** seed (`--seed=0` expands to a wall-clock seed) and the **resolved** fork (empty `--fork` resolves to the client's max) — the build version + git revision, the run's state root, and, when `--spec` is used, a content-addressed `state-actor-spec-<sha256>.yaml` sidecar written alongside it.

Re-run it into a fresh directory with the `reproduce` subcommand:

```bash
go run . reproduce --manifest /tmp/sa-geth/state-actor-manifest.json --db /tmp/sa-geth-repro/geth/chaindata
```

It replays the manifest's resolved flags (reading any spec from the sidecar — whose sha256 is verified first — not the original path), regenerates into `--db` (which must be a **fresh, empty or nonexistent** directory, distinct from the original), and verifies the new state root against the recorded one — exiting non-zero on mismatch. This works even for runs created with `--seed=0`, since the manifest captured the concrete seed. The reproduced datadir gets its own manifest too, with a `reproduced_from` field pointing back at the source manifest.

### Reproduce a CI failure locally

CI loads `examples/full-matrix-spec-feature.yaml` on top of `--seed=42 --target-size=100MB` (mirrored across all five `TestE2ESuite` constants). The auto-fill (mainnet-shaped 20 / 10 / 70 split) emits synthetic state to fill the headroom between the spec's projected cost and `--target-size` — the spec entities are written first, the auto-fill fills the gap. Re-run:

```bash
go test -run TestE2ESuite ./client/<client>/... -v
```

### Diagnose "wrong state root"

The cross-client genesis-root invariant says: same `--seed`, same spec, same client-policy → identical state root across all six MPT clients. If it diverges, see [`ARCHITECTURE.md#cross-client-determinism`](ARCHITECTURE.md#cross-client-determinism) for the full check-list (per-client calibration in [`internal/sizecal/doc.go`](../internal/sizecal/doc.go); canonical syscontract preamble; CI keystone job).

## Constraints & gotchas

- **Besu / reth / nethermind / ethrex / erigon require Docker** for the writer side (cgo dependencies; no native build on macOS). Only geth has a pure-Go writer.
- **`--seed=0` is a footgun**: `main.go` rewrites it to `time.Now().UnixNano()`, i.e. randomises. For determinism use any non-zero seed. Bench convention is `--seed=42`.
- **`--target-size` is required when `--spec` is not set**: drives the mainnet-shaped auto-fill (20 % account-trie / 10 % bytecode / 70 % storage) over the whole budget. With `--spec`, spec entities count first; the auto-fill fills the headroom on top. If the spec alone exceeds the budget, `internal/specbuild` truncates the entity list to the longest prefix that fits and emits a `--target-size … truncated spec at entity[N]` warning to stderr; no auto-fill runs in that case.
- **`--archive` is geth/reth only**: rejected for besu, nethermind, and ethrex; accepted but a no-op for erigon.
- **`--binary-trie` is geth-only** (EIP-7864).
- **When using `--spec` alone, omit `--target-size`** — that way no auto-fill runs on top and random EOAs can't collide with spec-derived addresses. Set both only when you want the auto-fill to pad the headroom.
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
| New spec template | [`internal/templates/doc.go`](../internal/templates/doc.go) | Implement `Template` in a new file in `internal/templates/`; `Register(yourTemplate)` in `init()`; add a section to [`SPEC.md`](SPEC.md); add a covering entity to [`examples/full-matrix-spec-feature.yaml`](../examples/full-matrix-spec-feature.yaml) so CI exercises it. Mirror `erc20.go`'s shape. |
| Change how spec entities resolve | [`internal/specbuild/doc.go`](../internal/specbuild/doc.go) | This is the seam between the YAML schema (parsed by `internal/spec`) and the writer-facing `[]PreAllocEntity` slice. Address modes live in `derive.go`. |
| Touch the YAML schema | [`internal/spec/doc.go`](../internal/spec/doc.go) | Pure data + parse + schema-time validation. Keep import-cycle-free with `internal/templates`. |
| Touch sizing / calibration | [`internal/sizecal/doc.go`](../internal/sizecal/doc.go) | One global `bytesPerSlot` constant per client; the CI invariance gate uses `NewFixed(64)` so test sizing can't mask a `Default()` drift. |
| Add or modify a client target | [`client/geth/doc.go`](../client/geth/doc.go) (template), [`client/reth/doc.go`](../client/reth/doc.go), [`client/besu/doc.go`](../client/besu/doc.go), [`client/nethermind/doc.go`](../client/nethermind/doc.go), [`client/ethrex/doc.go`](../client/ethrex/doc.go), [`client/erigon/doc.go`](../client/erigon/doc.go) | Add `client/<name>/` with a `Run` (or `RunCgo`) entry point, a `doc.go` documenting on-disk layout + pinned upstream version, and a `TestE2ESuite` driving Docker boot + the shared `e2e.RunSuitePhases` oracle. Wire into `internal/clientpolicy/policy.go`. |
| Work on the reth codec | [`internal/reth/doc.go`](../internal/reth/doc.go) | Byte-exact mirror of reth's MDBX schema + Compact codec. Pinned to a specific reth commit; updating requires regenerating `testdata/fixtures.json`. |
| Add a CLI flag | `main.go` + [`ARCHITECTURE.md`](ARCHITECTURE.md) | Declare in `main.go`, document in `--help` text, refresh the compact flag table in [`README.md`](../README.md). |
