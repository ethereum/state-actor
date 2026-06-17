# Regenerating genesis_dump.json

`genesis_dump.json` is a golden fixture produced by running the real ethrex
`add_initial_state` path against `genesis.json` and dumping every RocksDB
column family as hex-encoded JSON. It must be regenerated whenever the ethrex
storage schema changes.

## Pinned release

```
lambdaclass/ethrex @ v16.0.0
```

The state-bearing CFs (`account_trie_nodes`, `storage_trie_nodes`,
`account_codes`, `account_code_metadata`) are byte-identical from v13.0.0
(commit 318ec2888) through v16.0.0 — verified by regenerating at v16 and diffing
against the v15-generated dump (only `chain_data[0x80]` changed, to add
`osakaTime`). v16 bumped `STORE_SCHEMA_VERSION` to 3, but the v2→v3 migration
only rewrites `RECEIPTS`/`TRANSACTION_LOCATIONS` (both empty at genesis), so the
golden CFs are unaffected. v16 also matches the e2e boot pin (e2e_test.go).

Any ethrex commit that bumps `STORE_SCHEMA_VERSION` or changes the key layout
of `account_trie_nodes`, `storage_trie_nodes`, `account_codes`, or
`account_code_metadata` requires regenerating the dump and re-reviewing the
Go codec in `internal/ethrex/`.

## Steps

1. Check out ethrex at the pinned commit:

   ```sh
   git clone https://github.com/lambdaclass/ethrex
   cd ethrex
   git checkout v16.0.0
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
- This approach mirrors the regeneration note in `internal/reth/doc.go`.
