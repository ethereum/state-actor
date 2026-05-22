# Example specs

Each YAML in this directory demonstrates a slice of the `--spec` system. Pick by intent, not by feature.

## Gallery

| File | Demonstrates | Use when |
|---|---|---|
| [`spec-minimal.yaml`](spec-minimal.yaml) | 1 EOA with explicit address + balance | Smoke-testing `--spec`; the smallest possible example |
| [`spec-erc20-mixed-sizes.yaml`](spec-erc20-mixed-sizes.yaml) | 3 ERC-20 tokens with varying `approximate_size_bytes` (+ 5 EIP-7702 delegating EOAs) | Token-state experiments with mixed storage footprints |
| [`spec-eoa-bloat.yaml`](spec-eoa-bloat.yaml) | EIP-7702 delegating EOAs with synthesized storage | EOA delegation scenarios |
| [`full-matrix-spec-feature.yaml`](full-matrix-spec-feature.yaml) | Every spec feature in one file (22 entities — all 3 templates, all 3 address modes, EIP-7702 markers, raw bytecode, balance/nonce overrides, explicit + bulk ERC-20 owners/allowances, zero-balance EOA) | Reproducing CI; exercising every spec capability |

`test-genesis.json` in this directory is a separate artifact — a geth genesis file used by `docs/KURTOSIS.md`'s legacy integration path, not a `--spec` example.

## Choose your spec

For every feature in practice, read [`full-matrix-spec-feature.yaml`](full-matrix-spec-feature.yaml) — it covers every combination. The intent → entity-# index is in [`docs/SKILL.md#canonical-spec-reference`](../docs/SKILL.md#canonical-spec-reference). Use one of the smaller files (`spec-minimal.yaml`, `spec-erc20-mixed-sizes.yaml`, `spec-eoa-bloat.yaml`) when you want a focused example rather than the full matrix.

For the schema (parser rules, address resolution algorithm, validation), see [`docs/SPEC.md`](../docs/SPEC.md).

## How to use a spec

```bash
state-actor --client=geth --db=/tmp/sa-spec \
  --spec=examples/spec-minimal.yaml \
  --accounts=0 --contracts=0
```

`--accounts=0 --contracts=0` suppresses synthetic fill so the only entities written are those declared in the spec — recommended whenever you care about exact addresses, since name- and position-derived spec addresses can otherwise collide with randomly generated synthetic entities.

## Field-by-field cheatsheet

When adapting `full-matrix-spec-feature.yaml` (or any of the others) to a new shape, these are the fields you'll touch most often:

| To change | Field to edit | Notes |
|---|---|---|
| Token size | `entities[N].approximate_size_bytes` | Resolved to a slot count via `internal/sizecal` (±25 % accuracy) |
| Token name / symbol | `entities[N].parameters.{name,symbol}` | ≤ 31 bytes each (OZ v5 short-string limit) |
| Token decimals | `entities[N].parameters.decimals` | Must be `18` for the `erc20` template |
| Owner balances | `entities[N].parameters.owners` (explicit) and/or `total_owners` (bulk random) | Both can coexist; see entity 9 in `full-matrix-spec-feature.yaml` |
| Allowances | `entities[N].parameters.allowances` and/or `total_allowances` | Same explicit + bulk pattern as `owners` |
| Address mode | Set `address:` (explicit), set `name:` only (derived), or omit both (position-derived) | See `internal/specbuild/doc.go` for the derivation rules |
| Spec entity nonce | `entities[N].nonce` | Default 0; ERC-20 floors to ≥ 1 |
| EIP-7702 delegate | `entities[N].code` set to `0xef0100` + 20-byte delegate address (40 hex chars) | All hex; no string concatenation in YAML |
| Raw contract bytecode | `entities[N].code` (omit `template:`) | Must satisfy EIP-170 (≤ 24576 bytes) |
| Balance as hex | `balance: "0xde0b6b3a7640000"` | Decimal also accepted; always quoted (YAML scalar resolution would otherwise lose precision) |
