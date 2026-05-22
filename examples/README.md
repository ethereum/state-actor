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

- "I just want to see `--spec` work" → `spec-minimal.yaml`.
- "I want ERC-20 tokens of specific sizes" → `spec-erc20-mixed-sizes.yaml`.
- "I want bloated 7702 EOAs" → `spec-eoa-bloat.yaml`.
- "I want to exercise every feature at once" → `full-matrix-spec-feature.yaml`.

For the full schema (entity kinds, address resolution modes, template parameters, validation rules), see [`docs/SPEC.md`](../docs/SPEC.md).

## How to use a spec

```bash
state-actor --client=geth --db=/tmp/sa-spec \
  --spec=examples/spec-minimal.yaml \
  --accounts=0 --contracts=0
```

`--accounts=0 --contracts=0` suppresses synthetic fill so the only entities written are those declared in the spec — recommended whenever you care about exact addresses, since name- and position-derived spec addresses can otherwise collide with randomly generated synthetic entities.
