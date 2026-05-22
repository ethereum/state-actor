# 300 GB Bloat Plan — Superseded

> **This document is historical.** The original plan walked through
> calibrating per-entity byte costs and hand-tuning `--accounts`,
> `--contracts`, `--max-slots`, `--min-slots`, `--code-size`, and
> `--distribution` to hit a 20/70/10 split at 300 GB. The whole
> calibration loop is now handled by the `internal/autofill` module
> (see `internal/sizecal/factors.go` for the constants).

## Today's One-Liner

```bash
state-actor \
    --client geth \
    --db ./bloated-chaindata \
    --target-size 300GB \
    --binary-trie \
    --seed 42 \
    --verbose
```

That's it. The auto-fill emits:

- 20 % of the budget as account-trie leaves (EOAs + contract headers)
- 10 % as bytecode (truncated normal in `[1 KiB, 24 KiB]` centered at 5 KiB)
- 70 % as contract storage (truncated normal in `[1 KiB, 100 MiB]` whose
  mean is budget-derived)

EOAs randomize balance / nonce / EIP-7702 delegation independently. The
draws are deterministic per `--seed`.

If you need to overlay declarative entities (named ERC-20 contracts,
specific addresses, custom storage layouts) on top, point `--spec` at a
YAML file (see `docs/SPEC.md`); the auto-fill then targets `target_size`
minus the spec's projected cost.

## Hardware

- 16-32 GB RAM (with `--commit-interval`)
- 600 GB+ disk (Pebble compaction headroom)
- Multi-core CPU

## Verification

Walk the resulting DB with the per-client introspection tool — e.g. for
geth, `du -sh ./bloated-chaindata` should land within ±10 % of the target.

See `docs/CALIBRATION.md` for the trie-only byte-cost constants used by
the auto-fill math.
