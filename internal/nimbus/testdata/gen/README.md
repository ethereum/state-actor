# Regenerating genesis_dump.json

`genesis_dump.json` is a golden fixture produced by running the real nimbus
genesis path (`CommonRef.new` → `writeGenesis` + `initializeDb`) against
`genesis.json` and dumping the `AriVtx` and `KvtGen` column families as
hex-encoded JSON. It must be regenerated whenever nimbus's Aristo or Kvt
storage format changes.

## Pinned commit

```
status-im/nimbus-eth1 @ 2f0ae87cd   (image statusim/nimbus-eth1:master-2f0ae87)
```

A master build, not a tagged release: this commit ("Storage trie static vids",
#4797) changed the on-disk format — static storage-trie vertex IDs, the AccLeaf
`stoHint` byte, the `0x7e` SavedState trailer — and no release carries it.

The e2e boot image (`client/nimbus/e2e_test.go`), `internal/nimbus/doc.go` and
this fixture carry the same pin, and all three move together.

Regenerate when a new pin changes `aristo_blobify.nim` (vertex records, the
admin record), `aristo_vid.nim` / `aristo_constants.nim` (vertex-ID scheme),
`storage_types.nim` / `fcu_db.nim` (Kvt keys), or the column-family set.
Re-review the codec in `internal/nimbus/` in the same commit.

## Steps

1. Check out nimbus-eth1 at the pinned commit and build its dependencies:

   ```sh
   git clone https://github.com/status-im/nimbus-eth1
   cd nimbus-eth1
   git checkout 2f0ae87cd
   make -j"$(nproc)" update rocksdb
   ```

2. Copy the dump harness into the tree (it uses relative imports):

   ```sh
   mkdir -p tools/sa_dump
   cp /path/to/state-actor/internal/nimbus/testdata/gen/sa_dump.nim tools/sa_dump/
   ```

3. Build and run it, pointing it at `genesis.json`:

   ```sh
   ./env.sh nim c -d:release --out:build/sa_dump tools/sa_dump/sa_dump.nim
   build/sa_dump /path/to/state-actor/internal/nimbus/testdata/gen/genesis.json \
       /tmp/sa-nimbus-datadir /tmp/genesis_dump.json
   ```

4. Copy the output back and remove the harness from the nimbus tree:

   ```sh
   cp /tmp/genesis_dump.json /path/to/state-actor/internal/nimbus/testdata/genesis_dump.json
   rm -r tools/sa_dump
   ```

5. Run the golden tests:

   ```sh
   go test ./internal/nimbus/                                   # structural: VerifyState over the fixture
   go test -tags cgo_nimbus -run TestGenesisDumpGolden ./client/nimbus/   # byte-exact vs the writer (Docker builder image)
   ```

## Notes

- `genesis.json` is the same 3-account fixture ethrex's golden uses (chain id
  1337, prague; one EOA, one contract with 3 storage slots, one code-only
  contract). Keep exactly ONE storage-bearing account: nimbus allocates
  storage-trie IDs in ledger Table-iteration order, so with one such account
  the stoID is `FIRST_DYNAMIC_VID` on both sides and the dump stays byte-exact.
- The AccLeaf `stoHint` byte is NOT reproducible: nimbus derives it from the
  order it inserted slots (an insertion-order-dependent heuristic), while
  state-actor uses the shallowest leaf level. Any value 1..9 is valid;
  `TestGenesisDumpGolden` normalises the byte on both sides before comparing.
- The `0x00‖hash` header row must equal the `headers` row of
  `internal/ethrex/testdata/genesis_dump.json` (same genesis parameters ⇒ same
  consensus header). If it does not, the header path — not the trie codec —
  has drifted.
- The datadir grows to a few MB; `/tmp` is fine.
