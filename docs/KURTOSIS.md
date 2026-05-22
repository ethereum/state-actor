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
    --accounts=0 --contracts=0 \
    --chain-id=32382 --fork=osaka --gas-limit=60000000

# 2. Package for Kurtosis.
tar -czf bloated-state.tar.gz -C ./bloated-chaindata .
```

In the participant config, mount the unpacked tree and switch geth to Pebble:

```yaml
participants:
  - el_type: geth
    el_image: ethereum/client-go:latest
    el_extra_params:
      - --db.engine=pebble
    el_extra_env_vars:
      SKIP_GETH_INIT: "true"
    el_extra_mounts:
      /pregenerated-state: "{{.StateArtifact}}"
```

State Actor wrote the genesis block as part of the database, so no `geth init` runs.

For reth / besu / nethermind participants, the equivalent mounts plus boot flags are in [`RUNBOOK.md`](RUNBOOK.md). The key difference is that `--db.engine=pebble` is geth-specific; reth boots from `--chain=<chainspec.json>`, besu from `--genesis-file=<besu-chainspec.json>`, nethermind from a `--config=boot.cfg`.

## Method 2 — Starlark module

For runs where you want the state generated as part of the Kurtosis package itself:

```python
load("github.com/nerolation/state-actor/integration/stategen_launcher.star",
     "generate_bloated_state")

def run(plan, args):
    chaindata = generate_bloated_state(
        plan,
        output_artifact_name = "bloated-chaindata",
        client               = "geth",                        # or reth/besu/nethermind
        spec_file            = "examples/spec-erc20-mixed-sizes.yaml",
        num_accounts         = 0,
        num_contracts        = 0,
        target_size          = "10GB",
        seed                 = 42,
        chain_id             = 32382,
    )
    # Pass `chaindata` to your EL participant as a pre-mount.
```

The module wraps `state-actor` in a Kurtosis service so the artifact is staged in-cluster and the dependency on genesis generation is explicit in the Starlark dag.

## Method 3 — custom client image

Bake the pre-generated state into a client image. Useful when the same devnet boots many times against the same state.

```dockerfile
FROM ethereum/client-go:latest
COPY bloated-chaindata /pregenerated-state
COPY geth-wrapper.sh /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/geth-wrapper.sh"]
```

`integration/geth-wrapper.sh` copies the pre-generated state into `$GETH_DATADIR` on first boot and execs `geth` with the original arguments. Reth / besu / nethermind don't have a wrapper today; the equivalent step is a one-shot `cp -r` in the image's entrypoint.

## Starlark module reference

```python
generate_bloated_state(
    plan,
    output_artifact_name,
    client               = "geth",          # "geth" | "reth" | "besu" | "nethermind"
    spec_file            = None,            # Path to a YAML state-spec; see docs/SPEC.md
    num_accounts         = 1000,            # Synthetic-fill EOAs (set to 0 with --spec)
    num_contracts        = 100,             # Synthetic-fill contracts (set to 0 with --spec)
    target_size          = None,            # e.g. "5GB"; soft cap on synthetic fill
    min_slots            = 1,
    max_slots            = 10000,
    distribution         = "power-law",
    seed                 = 1,               # Non-zero! seed=0 randomises (footgun)
    chain_id             = 1337,
    fork                 = None,            # Empty → latest supported by the chosen client
    gas_limit            = 30000000,
    tolerations          = [],
    node_selectors       = {},
)
```

Returns a Files artifact containing the generated database tree.

## Picking the right `--client`

The choice depends on which EL implementation your participant runs. Each client expects a different on-disk format:

| Participant `el_type` | `--client` |
|---|---|
| `geth` | `geth` |
| `reth` | `reth` |
| `besu` | `besu` |
| `nethermind` | `nethermind` |

State Actor refuses to produce a database whose format doesn't match the chosen client (`internal/clientpolicy/` validates this at parse time), so a mismatch surfaces immediately rather than as a confusing boot failure.

## Troubleshooting

### State-root mismatch on boot

Most commonly: the participant's `network_id` / `chain-id` doesn't match the `--chain-id` you passed to State Actor. Less commonly: a per-client calibration drift in `internal/sizecal/` (file a bug). Verify with:

```bash
cast chain-id --rpc-url <participant-rpc>
```

### Spec entity unreachable after boot

If `eth_getCode` returns `0x` for an entity whose name you set in the spec, it's almost always a collision: the synthetic-fill loop wrote a random EOA / contract at the same name-derived address. Re-run with `--accounts=0 --contracts=0`.

### Participant boots but `eth_blockNumber` stays at `0x0`

`besu` and `nethermind` participants don't self-mine — they need a CL driving the Engine API. The `ethereum-package` consensus-layer participants drive blocks automatically. If you're running State-Actor's output outside of `ethereum-package`, see [`RUNBOOK.md`](RUNBOOK.md) for the mock-CL pattern.

## See also

- [`RUNBOOK.md`](RUNBOOK.md) — per-client boot recipes (geth / reth / besu / nethermind).
- [`SPEC.md`](SPEC.md) — `--spec` YAML schema.
- [`examples/README.md`](../examples/README.md) — curated spec gallery.
