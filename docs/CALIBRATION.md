# CALIBRATION — what `--target-size` buys per client

`internal/sizecal/factors.go` has referenced this document since inception; it
now exists. It answers the question every large run eventually asks: *why is
the datadir bigger than my `--target-size`?*

## The contract

`--target-size` is converted to synthetic entity counts through ONE global
constant, `bytesPerSlot = 140` (plus `bytesPerAccount = 175`), which budgets
the **trie component only**. This is deliberate and load-bearing:

- **The constant must be identical across clients.** Same seed + same target ⇒
  same entity set ⇒ same genesis state root on every client — the
  `cross-client-genesis-root` CI gate pins exactly this. A per-client
  `bytesPerSlot` would give each client a different entity set and different
  roots; it is forbidden by design, not an oversight.
- Flat-state layers (Pebble snapshot, Bonsai flat, reth MDBX flat tables,
  Nethermind flat rows, ethrex FlatKeyValue) are **additional** and
  deliberately uncounted — they differ per client and cannot be budgeted by a
  shared constant.

So the honest number per client is the **realized/target factor** below, not a
change to the constant.

## Realized/target factors (measured, seed 42, autofill shape 20/10/70)

| client | factor | provenance |
|---|---|---|
| ethrex | **≈1.82×** | measured: 40 GB→73 GiB, 150 GB→273 GiB (bloatnet host, 2026-08) |
| geth | TBM | to be measured by the cross-client 40 GB comparison run |
| besu | TBM | 〃 |
| nethermind | TBM | 〃 |
| reth | TBM | 〃 |
| erigon | TBM | 〃 |

TBM entries are filled by the standing measurement protocol: same host, same
seed, `--target-size=40GB` per client, record `du -sh` of the produced datadir.

## Where ethrex's 1.82× comes from (byte model, reproduces measurement to ~8%)

```
1.82× = 1.00×  trie + code CFs      ← the sizecal budget, hit almost exactly
              (storage_trie_nodes realizes ≈132 B/slot vs the 140 budget;
               account_trie_nodes ≈172 B/account vs 175)
      + 0.74×  FlatKeyValue CFs     ← the budget's documented exclusion
              (storage_flatkeyvalue ≈31% of the DB: a 131-byte raw-nibble
               key per ~33-byte value; account_flatkeyvalue ≈10%)
      + 0.08×  RocksDB structure    (indexes, filters, restart arrays)
```

The FlatKeyValue layer is not optional bloat: upstream ethrex generates it
from the trie in a background task (`flatkeyvalue_generator`, store.rs) and a
steady-state node holds **both** layers; the writer pre-completes generation
(`misc_values["last_written"] = 0xff`) so the datadir is instantly usable.
The generator never deletes trie rows, so nothing here can be dropped without
producing a datadir no real node ever has.

## Transient disk on top of the final size (ethrex)

- **Sort spill:** ≈0.5× target under the datadir volume (moved off `/tmp`
  deliberately), freed before `Close()`.
- **Write amplification:** ladder-measured at the 40 GB target (73 GiB
  realized; whole-device sector deltas, so spill and filesystem overhead
  included): RocksDB's 256 MB `max_bytes_for_level_base` default wrote
  314 GiB physical; the 2 GiB default ships because it was fastest by wall
  (298 GiB, −24% time); the defer-everything variant wrote least (272 GiB)
  but pays it back in a serial Close mega-compaction. Override with
  `STATE_ACTOR_ETHREX_LEVELBASE_MIB`.

Rule of thumb for provisioning an ethrex run: **free disk ≈ 2.4× the target**
(1.82× final + ~0.5× spill co-resident at the Phase-2 peak), plus headroom for
the Close-time compaction transient.

## Spill sizing (all clients that use streamsort)

Entity-blob spill ≈ accounts×~90 B + slots×64 B + full contract code bytes;
for the standard autofill shape that lands at ≈0.5× target. besu, nethermind
and geth currently spill to `os.TempDir()` — see issue #130.
