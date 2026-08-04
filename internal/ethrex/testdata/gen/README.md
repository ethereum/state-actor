# Regenerating genesis_dump.json

`genesis_dump.json` is a golden fixture produced by running the real ethrex
`add_initial_state` path against `genesis.json` and dumping every RocksDB
column family as hex-encoded JSON. It must be regenerated whenever the ethrex
storage schema changes.

## Pinned commit

```
lambdaclass/ethrex @ 80bcc71
```

A pre-release commit, the one the ethpandaops `glamsterdam-devnet-7` image was
built from. `--version` reports `v22.0.0` here: the branch does not descend from
the v23.0.0 version bump, though its storage code is ahead of it. Don't take the
version string as a sign the pin is wrong.

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
   git checkout 80bcc71
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
