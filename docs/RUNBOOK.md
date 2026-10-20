# Runbook: booting each client against a state-actor database

state-actor writes a client-native database. The boot command for that database differs by client. This file lists the seven CI-verified recipes — one per client — extracted from `client/<c>/e2e_test.go` (and `client/reth/oracle_test.go`).

Every section here is size-agnostic. `--target-size=10MB` and `--target-size=1TB` invoke the same code path, and the boot commands are the same in both cases.

## Mental model

```
state-actor --client=X --db=/path …            (writes a client-native database)
        ↓
client X boots against /path                   (one command, X-specific)
        ↓
JSON-RPC on http://…:8545                      (cast / curl / spamoor / your test)
```

The per-client boot command is the only piece that varies. Pick your client below.

## Geth

Reference: `client/geth/e2e_test.go` (`TestE2ESuite`).

**Generate.** Geth has a pure-Go writer — no Docker required.

```bash
go run . \
  --client=geth \
  --db=/tmp/sa-geth/geth/chaindata \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

Note the `/geth/chaindata` suffix on `--db`: geth itself appends that path to its `--datadir`, so state-actor must write at exactly that location.

Unlike the other clients, geth does not need an external genesis file at boot — its chain config is persisted inside the Pebble DB (`rawdb.WriteChainConfig`), so the geth boot command below carries no `--genesis`/`--chain` flag. For parity/inspection, state-actor still drops an informational `geth-genesis.json` at the datadir root (e.g. `/tmp/sa-geth/geth-genesis.json`). It is **not** read at boot. Its `alloc` is empty — state is direct-written into the trie, not derived from `alloc` — so `geth init`-ing from it yields the same chain *config* but an empty state, not a copy of the generated DB.

**Boot.** Docker is one option; native geth works equally.

```bash
docker run -d \
  --name state-actor-geth \
  --user $(id -u):$(id -g) \
  -v /tmp/sa-geth:/data \
  -p 127.0.0.1:8545:8545 \
  ethereum/client-go:v1.17.6 \
  --datadir=/data \
  --db.engine=pebble \
  --networkid=1337 \
  --dev --dev.period=1 --dev.gaslimit=60000000 \
  --http --http.addr=0.0.0.0 --http.port=8545 \
  --http.api=eth,net,web3,txpool \
  --http.corsdomain='*' --http.vhosts='*'
```

`--db.engine=pebble` is required — state-actor writes Pebble, not LevelDB. `--dev --dev.period=1` puts geth into self-mining mode (PoA, 1 s blocks).

**Verify.**

```bash
cast chain-id  --rpc-url http://127.0.0.1:8545   # → 0x539 (1337)
cast block-number --rpc-url http://127.0.0.1:8545 # → 0x0 immediately, ticks up
```

**Amsterdam variant.** Add `--fork=amsterdam` to the generate command (Sepolia spec). Boot with `ethereum/client-go:v1.17.6` or later, as above. Earlier geth releases do not know the Amsterdam header fields and reject the genesis. Reference: `TestE2ESuiteAmsterdam`.

**Binary-trie variant.** If you generated with `--binary-trie=true` (EIP-7864, geth-only), boot geth with `--override.ubt=0` in addition to the flags above.

## Reth

Reference: `client/reth/oracle_test.go` (`TestE2ESuite`).

> [!WARNING]
> Pin the reth image to a digest. `--debug.skip-genesis-validation` only
> landed in paradigmxyz/reth#23919 (2026-05-06), and the Sepolia Amsterdam
> spec only in reth v2.7.0. A tag can move, so pin the digest too.
> `internal/reth/constants.go`
> (`PinnedRethImage` + `PinnedRethRelease`) is the source of truth — the
> recipe below mirrors the current pin.

**Generate.** Reth uses cgo (MDBX bindings) — run the published Docker image (or build `Dockerfile.reth` locally).

```bash
docker run --rm \
  -v /tmp/sa-reth:/data \
  ghcr.io/ethereum/state-actor-reth:main \
  --client=reth --db=/data \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout** (the boot command references these paths):

- `/data/db/mdbx.dat` — MDBX state tables (`PlainAccountState`, `HashedAccounts`, `PlainStorageState`, `HashedStorages`, `Bytecodes`, …)
- `/data/db/database.version` — reth schema version sentinel
- `/data/rocksdb/` — reth's RocksDB v2 history tables
- `/data/static_files/{headers,transactions,receipts,transaction-senders}/` — block-0 nippy-jar segments
- `/data/chainspec.json` — reth-format chainspec (referenced by `--chain` below)

**Boot.**

```bash
docker run -d \
  --name state-actor-reth \
  -v /tmp/sa-reth:/data \
  ghcr.io/paradigmxyz/reth:v2.7.0@sha256:a7f059d470878a5a59f3c575f3fc81b536d0d2d56d7a1cbed73e717a4f766336 \
  node --dev --dev.block-time=1s \
  --chain=/data/chainspec.json \
  --datadir=/data \
  --debug.skip-genesis-validation \
  --http --http.addr=0.0.0.0 --http.port=8545 \
  --http.api=eth,net,web3,txpool
```

`--debug.skip-genesis-validation` is required because state-actor writes synthetic state directly into MDBX rather than reconstructing it from `chainspec.alloc`. `--dev --dev.block-time=1s` is reth's self-mining equivalent of geth's `--dev.period=1`.

**Amsterdam variant.** Add `--fork=amsterdam` to the generate command (Sepolia spec). Boot with `ghcr.io/paradigmxyz/reth:v2.7.0` or later, as above. Earlier reth releases do not know the Sepolia Amsterdam spec. At boot, reth logs `Genesis hash mismatch with chainspec`. This warning is expected: the chainspec has an empty `alloc`, so its hash is the empty-state genesis hash, and `--debug.skip-genesis-validation` tells reth to use the DB header. Reference: `TestE2ESuiteAmsterdam` in `client/reth/oracle_test.go`.

**Operational hygiene** (apply at every size — these are MDBX requirements, not scale-conditional advice):

- Linux `vm.max_map_count` ≥ 1048576 — MDBX maps thousands of regions; the default 65530 will trip cryptic mmap failures.
  ```bash
  sudo sysctl -w vm.max_map_count=1048576
  ```
- `ulimit -n` ≥ 65535 — reth's `static_files` and ETL spill open many file descriptors.
  ```bash
  ulimit -n 65535
  ```

**Verify.**

```bash
cast chain-id  --rpc-url http://<container-ip>:8545   # → 0x539
cast block-number --rpc-url http://<container-ip>:8545 # → ticks up
```

## Besu

Reference: `client/besu/e2e_test.go` (`TestE2ESuite`).

> [!WARNING]
> Pin the image tag. Besu 26.x removed the `--miner-enabled` flag this
> recipe relies on for post-merge dev mode; the e2e suite pins
> `hyperledger/besu:25.11.0`, so should you. See `client/besu/doc.go` for
> the longer reasoning.

**Generate.** Besu uses cgo (RocksDB JNI bindings on the writer side) — run the published Docker image (or build `Dockerfile.besu` locally).

```bash
docker run --rm \
  -v /tmp/sa-besu:/data \
  ghcr.io/ethereum/state-actor-besu:main \
  --client=besu --db=/data \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout:**

- `/data/besu-chainspec.json` — Besu chainspec referenced by `--genesis-file`
- `/data/database/` — single RocksDB instance with 8 Bonsai column families (default + BLOCKCHAIN + ACCOUNT_INFO_STATE + CODE_STORAGE + ACCOUNT_STORAGE_STORAGE + TRIE_BRANCH_STORAGE + TRIE_LOG_STORAGE + VARIABLES)

**Boot.**

```bash
docker run -d \
  --name state-actor-besu \
  -v /tmp/sa-besu:/data \
  hyperledger/besu:25.11.0 \
  --data-path=/data \
  --genesis-file=/data/besu-chainspec.json \
  --network-id=1337 \
  --data-storage-format=BONSAI \
  --genesis-state-hash-cache-enabled \
  --rpc-http-enabled --rpc-http-host=0.0.0.0 --rpc-http-port=8545 \
  --rpc-http-api=ETH,NET,WEB3 \
  --host-allowlist=all \
  --min-gas-price=0 \
  --engine-rpc-enabled --engine-rpc-port=8551 \
  --engine-host-allowlist=all \
  --engine-jwt-disabled
```

Four besu-specific subtleties:

- `--genesis-state-hash-cache-enabled` tells besu to trust the stored state root rather than recompute it from `chainspec.alloc` — required, since state-actor writes synthetic state directly into RocksDB.
- `--data-storage-format=BONSAI` matches state-actor's writer layout.
- Besu has **no native post-Merge dev mode** (clique is broken post-Shanghai; the legacy `--miner-enabled` path was removed in 26.4.0). Block production must be driven via the Engine API by a mock consensus layer. The state-actor repo's `internal/engineapi/` package provides a Go-side driver; in CI, `internal/e2e_testing.StartEngineDriver` drives blocks via `engine_forkchoiceUpdated` / `getPayload` / `newPayload`.
- besu accepts `engine_forkchoiceUpdated` on this isolated node only with **p2p enabled** (the default — the boot command above does not disable it). Its synchronizer must run to register the post-merge head as in-sync; with `--p2p-enabled=false` it never does, so besu answers `SYNCING` to every forkchoice update. Keep p2p on (peers stay at zero anyway).

The flags above match `host-allowlist=all` (literal `all`, not `*` — `*` would glob-expand against `/opt/besu/` in the image's entrypoint).

**Amsterdam.** `--fork=amsterdam` follows the Sepolia Amsterdam spec. Boot it on `hyperledger/besu:26.9.0` or later, with the same flags. 26.9.0 is the first besu release with Amsterdam (Sepolia `amsterdamTime` 1791294816). 25.11.0 does not know the fork. The chainspec gets `amsterdamTime` and the EIP-8282 builder request contract addresses. Besu then rebuilds the genesis header with `balHash` and `slotNumber`, so the hash matches the stored genesis. Drive blocks with `engine_forkchoiceUpdatedV4` (with `slotNumber`), `engine_getPayloadV6` and `engine_newPayloadV5` (`engineapi.ForkAmsterdam`). `TestE2ESuiteAmsterdam` covers this path.

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

Block-number stays at 0 until a consensus layer drives `engine_forkchoiceUpdated`.

## Nethermind

Reference: `client/nethermind/e2e_test.go` (`TestE2ESuite`).

**Generate.** Nethermind uses cgo (RocksDB) — run the published Docker image (or build `Dockerfile.nethermind` locally).

```bash
docker run --rm \
  -v /tmp/sa-neth:/data \
  ghcr.io/ethereum/state-actor-nethermind:main \
  --client=nethermind --db=/data \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout:**

- `/data/parity-chainspec.json` — Parity-format chainspec referenced by the boot config
- `/data/<8 RocksDB instances>` — `state`, `code`, `blocks`, `headers`, `blockNumbers`, `blockInfos`, `receipts`, plus a `flat` column DB (Nethermind's flat-state backend)

**Pre-launch: write a boot.cfg.** Nethermind doesn't take chainspec + datadir as command-line flags — it reads them from a JSON config. Write this to `/data/boot.cfg` (substituting `/data/parity-chainspec.json` for `%CHAINSPEC%` and `/data` for `%BASEDB%`):

```json
{
  "Init": {
    "EnableUnsecuredDevWallet": true,
    "KeepDevWalletInMemory": true,
    "DiscoveryEnabled": false,
    "PeerManagerEnabled": false,
    "ChainSpecPath": "%CHAINSPEC%",
    "BaseDbPath": "%BASEDB%",
    "MemoryHint": 256000000
  },
  "Sync": {
    "PivotNumber": 0
  },
  "TxPool": { "Size": 128, "BlobsSupport": "Disabled" },
  "Network": { "ActivePeersMaxCount": 0 },
  "JsonRpc": {
    "Enabled": true,
    "Timeout": 20000,
    "Host": "0.0.0.0",
    "Port": 8545,
    "EnabledModules": ["Eth", "Net", "Web3"],
    "EngineHost": "0.0.0.0",
    "EnginePort": 8551,
    "EngineEnabledModules": ["Engine", "Eth", "Net", "Web3", "Subscribe"],
    "UnsecureDevNoRpcAuthentication": true
  },
  "Metrics": { "Enabled": false },
  "Merge": { "Enabled": true, "TerminalTotalDifficulty": "0" },
  "Mining": { "Enabled": false }
}
```

The full template is in `client/nethermind/e2e_test.go`'s `nethermindE2EConfigTemplate`. `Init.BaseDbPath` must equal the datadir; its default is `<datadir>/nethermind_db/<network>/`, which is NOT where state-actor writes — omitting it silently creates an empty DB and `eth_getBlockByNumber("0x0").stateRoot` returns `0x56e81f17…` (empty-trie hash).

**Boot.**

```bash
docker run -d \
  --name state-actor-neth \
  -v /tmp/sa-neth:/data \
  nethermind/nethermind:1.39.0 \
  --config=/data/boot.cfg \
  --FlatDb.Enabled=true \
  --log=Info
```

state-actor writes Nethermind's **flat** state layout, so the boot MUST pass
`--FlatDb.Enabled=true` — without it Nethermind selects the patricia backend,
finds an empty `state` DB, and `eth_getBalance` returns zero for every account.
A correct boot logs `State backend: flat (existing flat DB detected).`; a
missing flag logs `State backend: patricia (existing patricia state detected).`

Like besu, Nethermind has no native post-Merge dev mode in our setup — `Merge.Enabled=true` + `TerminalTotalDifficulty=0` + the Ethash-from-genesis chainspec hand block production to MergePlugin via the Engine API. Drive blocks from an external CL mock (the state-actor repo's `internal/engineapi/`).

**Amsterdam (`--fork=amsterdam`, Sepolia spec).** Boot an Amsterdam genesis on `nethermind/nethermind:2.1.0` or later. Nethermind 1.39.x has no Amsterdam support. The boot command and `boot.cfg` do not change. The written chainspec sets these keys to `0x0`, as Nethermind's Sepolia chainspec does at the Amsterdam timestamp:

- `eip2780TransitionTimestamp`, `eip7708TransitionTimestamp`, `eip7778TransitionTimestamp`
- `eip7843TransitionTimestamp` (header `slotNumber`) and `eip7928TransitionTimestamp` (header `blockAccessListHash`)
- `eip7954TransitionTimestamp`, `eip7976TransitionTimestamp`, `eip7981TransitionTimestamp`
- `eip8024TransitionTimestamp`, `eip8037TransitionTimestamp`, `eip8038TransitionTimestamp`
- `eip8246TransitionTimestamp`, `eip8282TransitionTimestamp`

Nethermind builds its own genesis header from these keys and compares its hash with the stored header. Thus the keys and the stored header must agree. Drive Amsterdam blocks with `engineapi.ForkAmsterdam` (`engine_forkchoiceUpdatedV4`, `engine_getPayloadV6`, `engine_newPayloadV5`). `TestE2ESuiteAmsterdam` in `client/nethermind/e2e_test.go` is the reference.

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

## Ethrex

Reference: `client/ethrex/e2e_test.go` (`TestE2ESuite`, `TestE2ESuiteAmsterdam`). Run both with `make test-ethrex-suite SPAMOOR=<path>`.

**Generate.** ethrex uses cgo (RocksDB bindings via grocksdb) — build via Docker.

```bash
docker build -f Dockerfile.ethrex -t state-actor-ethrex .
docker run --rm \
  -v /tmp/sa-ethrex:/data \
  state-actor-ethrex \
  ./state-actor \
  --client=ethrex --db=/data \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout:**

- `/data/` — single RocksDB instance with 22 column families (see `internal/ethrex/constants.go` for the full list)
- `/data/metadata.json` — `{"schema_version": 4}`, required by ethrex `Store::new`
- `/data/ethrex-genesis.json` — full genesis JSON; pass via `--network` when booting

**Boot path.** ethrex's `add_initial_state` short-circuits when `canonical_block_hashes[0]` already resolves to a matching genesis header hash — state-actor writes that row, so ethrex skips state-trie recomputation at boot. Required boot flags (validated by the e2e suite, Phase 4): `--network <ethrex-genesis.json>`, `--datadir <dir>`, `--skip-genesis-validation` (trust the written stateRoot rather than recompute from the empty-alloc sidecar; needs lambdaclass/ethrex#6783, in releases ≥ v16.0.0), and `--syncmode full`. The `--syncmode full` flag is mandatory for engine-driven block production: in the default snap mode ethrex's fork-choice handler returns `SYNCING` with a null `payloadId` for every `engine_forkchoiceUpdated`, so the mock CL can never obtain a payload to build. The pattern follows the besu/nethermind Engine API approach: boot the node, then drive blocks via `engine_forkchoiceUpdated` (ethrex also mandates an authrpc JWT, signed by the driver).

**Amsterdam.** With `--fork=amsterdam` the sidecar and `chain_data` carry `amsterdamTime: 0`, and the genesis header carries `blockAccessListHash` (EIP-7928) and `slotNumber = 0` (EIP-7843). Amsterdam gets no `blobSchedule` entry: ethrex uses the `osaka` entry (6/9), the same values that geth gives Amsterdam. To build blocks, send `engine_forkchoiceUpdatedV4`. Its payload attributes must include `slotNumber` and `targetGasLimit`. ethrex (≥ v20.0.0, execution-apis#796) rejects V4 attributes without `targetGasLimit` with `-38003 Invalid payload attributes`. geth accepts them without it.

**Image pin.** `ghcr.io/lambdaclass/ethrex:30.0.0` (commit `80b774a`), the release that schedules Amsterdam on Sepolia (`1791294816`) and Hoodi. `ETHREX_IMAGE` overrides the pin. The on-disk format is the same as at the previous pin (`ethpandaops/ethrex:glamsterdam-devnet-7`, commit `80bcc71`): `schema_version` 4, the same 22 column families, and the JUMPDEST bitmap in `account_codes`. `internal/ethrex/testdata/gen/README.md` explains how to regenerate the golden fixture when the pin moves.

**Memory at boot.** ethrex's shared RocksDB block cache defaults to 12 GiB and holds index + bloom-filter blocks. The cache fills lazily, but on a large DB a memory-capped container can be OOM-killed as it fills — cap it with `--rocksdb.block-cache-size <bytes>` on memory-constrained hosts. Boot-side knob only; no bearing on the bytes state-actor writes.

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

## Erigon

Reference: `client/erigon/e2e_test.go` (`TestE2ESuite`).

**Generate.** erigon uses cgo (mdbx-go + a vendored commitment lib) — build via Docker.

```bash
docker build -f Dockerfile.erigon -t state-actor-erigon .
docker run --rm \
  -v /tmp/sa-erigon:/data \
  state-actor-erigon \
  ./state-actor \
  --client=erigon --db=/data \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout:**

- `/data/snapshots/` — Erigon v3 flat snapshot `.kv` files (accounts / storage / code / commitment) plus their accessors (`.bt` / `.kvi` / `.kvei`); the state bloat lives here
- `/data/chaindata/` — a minimal MDBX (genesis header with a patched `stateRoot` + chain config); it stays minimal, the bloat does not live here
- state-actor runs stock upstream erigon as a CLI (`erigon init`, then the daemon) — no upstream patch

**Boot path.** Commitment continuability across genesis → first-live-block uses the "fat genesis" construction (`MaxTxNum[0]=StepSize-1`, mirrored in the genesis body's `TxCount` and `Sequence[EthTx]` so erigon ≥2e41aa8308's body-derived rebuild on `FCU(head=genesis)` reproduces it, plus a commitment anchor — see `client/erigon/genesis_patch.go` + `snapshot_cgo.go`) so the chain advances past block 2. erigon mandates an authrpc JWT (it has no `--authrpc.jwt-disabled` flag). Required boot flags (validated by the e2e suite): `--externalcl` (drive blocks via the engine API — v3.4.2's `--chain dev` is broken), `--snap.stop` + `--snap.state.stop` (don't gate boot on the snapshot stages), and `--authrpc.jwtsecret <file>`. The pattern follows the besu/nethermind Engine API approach: boot the node, then drive blocks via `engine_forkchoiceUpdated` (the driver signs the authrpc JWT).

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

**Amsterdam variant.** Add `--fork=amsterdam` to the generate command (Sepolia spec). Boot the erigon binary that `Dockerfile.erigon` builds (the pinned commit `546790a6a7`, which ships the Sepolia `amsterdamTime`), with the flags above. The engine driver must use `engine_forkchoiceUpdatedV4` with `slotNumber` and `targetGasLimit` (erigon rejects V4 attributes without either, with -38003), `engine_getPayloadV6` and `engine_newPayloadV5`. `erigon init` writes the Amsterdam header fields (`blockAccessListHash` = the empty-list hash, `slotNumber` = 0); the stateRoot patch keeps them. Reference: `TestE2ESuiteAmsterdam`.

## Nimbus

Reference: `client/nimbus/e2e_test.go` (`TestE2ESuite`).

**Generate.** nimbus uses cgo (RocksDB bindings via grocksdb) — build via Docker.

```bash
docker build -f Dockerfile.nimbus -t state-actor-nimbus .
docker run --rm \
  -v /tmp/sa-nimbus:/data \
  state-actor-nimbus \
  --client=nimbus --db=/data \
  --target-size=100MB \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout:**

- `/data/ecdb/` — the single RocksDB nimbus opens under its `--data-dir` (no flag for it): `AriVtx` holds every account- and storage-trie vertex in nimbus's Aristo format (static vertex IDs for the top 8 levels, dynamic IDs below, every branch carrying its Merkle key) plus the admin record under the empty key; `KvtGen` holds the genesis header / total difficulty / number→hash / fcuHead / canonical-head rows and `0x06‖codeHash → bytecode`; `KvtSync` and `default` are empty
- `/data/nimbus-genesis.json` — Geth-style genesis with an EMPTY alloc; pass via `--network` when booting

**Boot path.** Nimbus rebuilds genesis from `--network`'s alloc on every start and refuses a datadir whose block-0 hash differs (`preventLoadingDataDirForTheWrongNetwork`). The sidecar's alloc is empty — the state lives in `AriVtx` and the real root is in the stored header — so boot with the hidden `--debug-rewrite-datadir-id` flag, which skips that check: the analogue of ethrex's `--skip-genesis-validation`. With block 0 present nimbus never rewrites genesis, `initializeDb` sees the canonical-head row and skips, and ForkedChain loads base = block 0 (a "Cannot find previous FC state" warning at boot is expected). Every branch ships with its Merkle key, so boot does not walk the state (no `Writing computeKey cache` line). Required boot flags (validated by the e2e suite):

```bash
docker run -d --name nimbus --user 0:0 \
  -v /tmp/sa-nimbus:/data \
  statusim/nimbus-eth1:v0.4.3 \
  executionClient \
  --data-dir=/data \
  --network=/data/nimbus-genesis.json \
  --debug-rewrite-datadir-id \
  --rpc --rpc-api=eth \
  --http-address=0.0.0.0 --http-port=8545 \
  --engine-api --engine-api-address=0.0.0.0 --engine-api-port=8551 \
  --jwt-secret=/data/jwt.hex \
  --max-peers=0 --discv5=false --nat=none
```

`executionClient` selects the EL inside the combined `nimbus` binary (without it the image runs CL + EL). `--rpc-api` accepts only `eth`, `debug`, `admin` (`web3_*` / `net_*` are always served). Nimbus has no dev / self-mining mode, so the chain is engine-driven like besu / nethermind, and the engine API mandates a JWT: write 32 random bytes as bare hex to `/data/jwt.hex` before booting and sign engine calls with it (`internal/e2e_testing.StartEngineDriverWithJWT`). Nimbus chmods the data dir to 0700 at startup. The release images run as an unprivileged `user`, but state-actor writes the datadir and `jwt.hex` as root, so run nimbus with `--user 0:0`. Without it nimbus stops at boot with `jwt-secret=... File not accessible`.

**Image pin.** `statusim/nimbus-eth1:v0.4.3` (commit `6e4d83c1`). Use v0.4.2 or later: commit `2f0ae87cd` ("Storage trie static vids") changed the Aristo on-disk format (static storage-trie vertex IDs, the AccLeaf `stoHint` byte, the `0x7e` SavedState trailer), and v0.4.1 and earlier cannot read it. v0.4.3 has the same format as `2f0ae87cd`, the source of the codec and the golden fixture. `NIMBUS_IMAGE` overrides the pin in `TestE2ESuite` / `TestE2ESuiteAmsterdam` / `make test-nimbus-suite`. `internal/nimbus/testdata/gen/README.md` tells how to regenerate the golden fixture when the format changes.

**Amsterdam variant.** Add `--fork=amsterdam` to the generate command (Sepolia spec). Boot as above with nimbus-eth1 v0.4.2 or later (v0.4.2 scheduled the Sepolia Amsterdam activation). The sidecar then sets `amsterdamTime: 0` and a `blobSchedule.amsterdam` entry with the Prague values. The stored header carries `blockAccessListHash` (the empty-list hash) and `slotNumber = 0`, the same header nimbus builds from the sidecar. The engine driver must use `engine_forkchoiceUpdatedV4` (with `slotNumber`), `engine_getPayloadV6` and `engine_newPayloadV5`. Reference: `TestE2ESuiteAmsterdam`.

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `missing librocksdb` at build | cgo client built without the system RocksDB | Build via the per-client `Dockerfile.<client>` (see [Besu](#besu) / [Nethermind](#nethermind) / [Reth](#reth) / [Ethrex](#ethrex) / [Erigon](#erigon) / [Nimbus](#nimbus)) |
| Nimbus: `Data dir already initialized with other network configuration` | booted without `--debug-rewrite-datadir-id`, so nimbus rebuilt genesis from the empty-alloc sidecar and compared hashes | add the flag (see [Nimbus](#nimbus)) |
| Nimbus: boot stalls on `Writing computeKey cache` | a branch vertex was shipped without its Merkle key, so nimbus re-hashes the whole trie | regenerate; `internal/nimbus.VerifyState` (run by `TestNimbusGoldenStateRoot`) pins that every branch carries its key |
| Reth: `mmap: cannot allocate memory` | `vm.max_map_count` too low | `sudo sysctl -w vm.max_map_count=1048576` (see [Reth](#reth) operational hygiene) |
| Besu / Neth: empty `eth_blockNumber` indefinitely | No consensus layer driving the Engine API | Run a mock CL (see `internal/engineapi/`) or use `internal/e2e_testing.StartEngineDriver` ([Besu engine-API note](#besu), [Nethermind engine-API note](#nethermind)) |
| `eth_getCode` returns `0x` for a name-derived spec entity | Auto-fill collided with the derived address | Re-run without `--target-size` (no auto-fill) or with a smaller `--target-size` |
| Cross-client state-root divergence | Per-client `sizecal` drift, or missing canonical syscontracts | See [`ARCHITECTURE.md#cross-client-determinism`](ARCHITECTURE.md#cross-client-determinism); check `internal/clientpolicy/` calibration; ensure `syscontracts.AddCanonicalSystemContracts` ran |
| Reth: `database.version mismatch` | Boot image's reth doesn't match state-actor's pinned codec version | Pin the reth image tag; see `internal/reth/constants.go` ([Reth section](#reth)) |

## What's deliberately not in this document

No "at scale" section, no GB-tiered tuning tables, no per-client memory-profile tables, no wall-clock numbers. The library handles every size the same way; documentation that buckets by size goes stale within months and primes readers to expect different behaviour at different sizes that the code does not actually exhibit.
