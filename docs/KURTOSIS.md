# Kurtosis / ethereum-package integration

This guide explains how to use State Actor to pre-populate the execution-layer database of an [ethereum-package](https://github.com/ethpandaops/ethereum-package) participant in Kurtosis.

State Actor produces a client-native database that the EL client can boot against directly — no `init` step required. The integration boils down to: generate the DB, hand its directory to the participant as a pre-mounted volume, set the right boot flags. The exact boot flags differ by client; see [`RUNBOOK.md`](RUNBOOK.md) for per-client recipes.

## Method 1 — pre-generate, then mount

The simplest path: generate the DB once on your workstation, package it, mount it into the participant.

```bash
# 1. Generate a geth database with a curated spec.
state-actor --client=geth \
    --db=./bloated-chaindata/geth/chaindata \
    --spec=examples/spec-erc20-mixed-sizes.yaml \
    --chain-id=32382 --fork=osaka --gas-limit=60000000

# 2. Package for Kurtosis.
tar -czf bloated-state.tar.gz -C ./bloated-chaindata .
```

In the participant config, mount the unpacked tree and switch geth to Pebble. Pin the image to a tested tag (`v1.17.2` is what state-actor's own e2e suite uses; `latest` may drift):

```yaml
participants:
  - el_type: geth
    el_image: ethereum/client-go:v1.17.2
    el_extra_params:
      - --db.engine=pebble
    el_extra_env_vars:
      SKIP_GETH_INIT: "true"
    el_extra_mounts:
      /pregenerated-state: "{{.StateArtifact}}"
```

State Actor wrote the genesis block as part of the database, so no `geth init` runs.

For reth / besu / nethermind participants, the equivalent mounts plus boot flags are in [`RUNBOOK.md`](RUNBOOK.md). The key difference is that `--db.engine=pebble` is geth-specific; reth boots from `--chain=<chainspec.json>`, besu from `--genesis-file=<besu-chainspec.json>`, nethermind from a `--config=boot.cfg`.

## Method 2 — Starlark module (legacy; geth-only)

> [!WARNING]
> `integration/stategen_launcher.star` predates the multi-client + `--spec`
> work AND the autofill rewrite. It calls a `stategen:latest` Docker image
> (not built by this repo) and passes flags that no longer exist on
> `state-actor` (`--genesis`, `--batch-size`, `--accounts`, `--contracts`,
> `--max-slots`, `--min-slots`, `--distribution`). It will not run against
> the current binary as-shipped. Use Method 1 (pre-generate, then mount)
> until the Starlark module is rewritten.

The module's legacy signature (preserved so existing Kurtosis packages keep parsing) is:

```python
generate_bloated_state(
    plan,
    output_artifact_name,
    genesis_artifact = None,
    num_accounts     = 10000,    # removed flag — has no effect
    num_contracts    = 5000,     # removed flag — has no effect
    max_slots        = 10000,    # removed flag — has no effect
    min_slots        = 100,      # removed flag — has no effect
    distribution     = "power-law",  # removed flag — has no effect
    seed             = 0,
    binary_trie      = False,
    tolerations      = [],
    node_selectors   = {},
)
```

If you need the multi-client surface from a Kurtosis package today, drive `state-actor` from a `plan.run_sh(...)` step with the current CLI directly (see [`docs/SKILL.md`](SKILL.md) for the flag set), or use Method 1 above.

## Method 3 — custom client image

Bake the pre-generated state into a client image. Useful when the same devnet boots many times against the same state.

```dockerfile
FROM ethereum/client-go:v1.17.2
COPY bloated-chaindata /pregenerated-state
COPY geth-wrapper.sh /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/geth-wrapper.sh"]
```

`integration/geth-wrapper.sh` copies the pre-generated state into `$GETH_DATADIR` on first boot and execs `geth` with the original arguments. Reth / besu / nethermind don't have a wrapper today; the equivalent step is a one-shot `cp -r` in the image's entrypoint.

## Picking the right `--client`

`--client` must match the participant's `el_type` 1:1 (`geth` ↔ `geth`, etc.). `internal/clientpolicy/` validates the flag mix at parse time, so a mismatch fails fast at generation rather than as a confusing boot failure.

## Troubleshooting

### State-root mismatch on boot

Most commonly: the participant's `network_id` / `chain-id` doesn't match the `--chain-id` you passed to State Actor. Less commonly: drift in the single global `bytesPerSlot` constant in `internal/sizecal/` versus the CI fixed-sizer (file a bug). Verify with:

```bash
cast chain-id --rpc-url <participant-rpc>
```

### Spec entity unreachable after boot

If `eth_getCode` returns `0x` for an entity whose name you set in the spec, it's almost always a collision: the auto-fill wrote a random EOA / contract at the same name-derived address. Re-run without `--target-size` (so no auto-fill runs at all), or with a smaller `--target-size` so the spec entities exhaust the headroom first.

### Participant boots but `eth_blockNumber` stays at `0x0`

`besu` and `nethermind` participants don't self-mine — they need a CL driving the Engine API. The `ethereum-package` consensus-layer participants drive blocks automatically. If you're running State-Actor's output outside of `ethereum-package`, see [`RUNBOOK.md`](RUNBOOK.md) for the mock-CL pattern.

## See also

- [`RUNBOOK.md`](RUNBOOK.md) — per-client boot recipes (geth / reth / besu / nethermind).
- [`SPEC.md`](SPEC.md) — `--spec` YAML schema.
- [`examples/README.md`](../examples/README.md) — curated spec gallery.
