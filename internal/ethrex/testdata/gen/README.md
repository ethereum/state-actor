# Regenerating genesis_dump.json

`genesis_dump.json` is a golden fixture produced by running the real ethrex
`add_initial_state` path against `genesis.json` and dumping every RocksDB
column family as hex-encoded JSON. It must be regenerated whenever the ethrex
storage schema changes.

## Pinned commit

```
lambdaclass/ethrex @ v30.0.0 (80b774a)
```

The previous pin was commit 80bcc71 (the ethpandaops `glamsterdam-devnet-7`
build). The v30.0.0 dump differs from that fixture only in three places, none of
them a format change on the state-actor side:

- `account_codes` values carry the JUMPDEST bitmap that the writer already
  emits. The old fixture still had the older RLP list of offsets.
- `chain_data[0x80]` gains `"lstarTime":null`. The golden test checks only that
  this value is valid JSON, because ethrex rewrites it from `--network` at boot.
- The `state_history` CF is listed (empty).

The e2e boot image (`client/ethrex/e2e_test.go`) and `Tables`
(`internal/ethrex/constants.go`) carry the same pin, and all three move
together. `Tables` must never list a CF the pinned build lacks; ethrex's
`drop_obsolete_cfs` drops any CF missing from its own `TABLES`.

Regenerate when a new pin bumps `STORE_SCHEMA_VERSION`, adds or removes a column
family, or changes the key layout of `account_trie_nodes`, `storage_trie_nodes`,
`account_codes`, or `account_code_metadata`. Re-review the codec in
`internal/ethrex/` in the same commit.

## Steps

1. Check out ethrex at the pinned commit:

   ```sh
   git clone https://github.com/lambdaclass/ethrex
   cd ethrex
   git checkout v30.0.0
   ```

2. Copy the dump harness into ethrex's examples directory:

   ```sh
   cp /path/to/state-actor/internal/ethrex/testdata/gen/sa_dump.rs \
       crates/storage/examples/sa_dump.rs
   ```

3. Run the harness, pointing it at `genesis.json`:

   ```sh
   cargo run -p ethrex-storage --features rocksdb --example sa_dump -- \
       /path/to/state-actor/internal/ethrex/testdata/gen/genesis.json \
       /tmp/ethrex-sa-datadir \
       /tmp/genesis_dump.json
   ```

4. Copy the output back:

   ```sh
   cp /tmp/genesis_dump.json \
       /path/to/state-actor/internal/ethrex/testdata/genesis_dump.json
   ```

5. Run the golden tests to confirm all rows still match:

   ```sh
   go test ./internal/ethrex/...
   ```

   All tests must pass before committing the updated fixture.

## Notes

- `/tmp` is tmpfs on some systems; use a disk-backed path for the datadir if
  disk space is limited (the RocksDB datadir grows to ~10 MB for this genesis).
- The `sa_dump.rs` harness reopens the store read-only after a 200 ms sleep to
  let the background FKV generator thread exit cleanly. Do not remove that sleep.
- `genesis.json` sets chainId 1337 with one EOA, one storage contract (3 slots),
  and one JUMPDEST contract. Changing `genesis.json` also invalidates the dump.
