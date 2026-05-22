# Runbook: booting each client against a state-actor database

state-actor writes a client-native database. The boot command for that database differs by client. This file lists the four CI-verified recipes — one per client — extracted from `client/<c>/e2e_test.go` (and `client/reth/oracle_test.go`).

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
  --accounts=100 --contracts=15000 \
  --code-size=128 --min-slots=5 --max-slots=50 \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

Note the `/geth/chaindata` suffix on `--db`: geth itself appends that path to its `--datadir`, so state-actor must write at exactly that location.

**Boot.** Docker is one option; native geth works equally.

```bash
docker run -d \
  --name state-actor-geth \
  --user $(id -u):$(id -g) \
  -v /tmp/sa-geth:/data \
  -p 127.0.0.1:8545:8545 \
  ethereum/client-go:v1.17.2 \
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

**Binary-trie variant.** If you generated with `--binary-trie=true` (EIP-7864, geth-only), boot geth with `--override.verkle=0` in addition to the flags above.

## Reth

Reference: `client/reth/oracle_test.go` (`TestE2ESuite`).

> [!WARNING]
> Pin the reth image to a digest. `--debug.skip-genesis-validation` only
> landed in paradigmxyz/reth#23919 (2026-05-06); reth's `:nightly` tag
> overwrites daily and is not safe. `internal/reth/constants.go`
> (`PinnedRethImage` + `PinnedRethRelease`) is the source of truth — the
> recipe below mirrors the current pin.

**Generate.** Reth uses cgo (MDBX bindings) — build via Docker.

```bash
docker build -f Dockerfile.reth -t state-actor-reth .
docker run --rm \
  -v /tmp/sa-reth:/data \
  state-actor-reth \
  ./state-actor \
  --client=reth --db=/data \
  --accounts=100 --contracts=15000 \
  --code-size=128 --min-slots=5 --max-slots=50 \
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
  ghcr.io/paradigmxyz/reth:nightly@sha256:e528857e5e9ebc2c6cb99f28436e70ded38ca905629f00afc98d186e27d206e0 \
  node --dev --dev.block-time=1s \
  --chain=/data/chainspec.json \
  --datadir=/data \
  --debug.skip-genesis-validation \
  --http --http.addr=0.0.0.0 --http.port=8545 \
  --http.api=eth,net,web3,txpool
```

`--debug.skip-genesis-validation` is required because state-actor writes synthetic state directly into MDBX rather than reconstructing it from `chainspec.alloc`. `--dev --dev.block-time=1s` is reth's self-mining equivalent of geth's `--dev.period=1`.

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

**Generate.** Besu uses cgo (RocksDB JNI bindings on the writer side) — build via Docker.

```bash
docker build -f Dockerfile.besu -t state-actor-besu .
docker run --rm \
  -v /tmp/sa-besu:/data \
  state-actor-besu \
  ./state-actor \
  --client=besu --db=/data \
  --accounts=100 --contracts=15000 \
  --code-size=128 --min-slots=5 --max-slots=50 \
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

Three besu-specific subtleties:

- `--genesis-state-hash-cache-enabled` tells besu to trust the stored state root rather than recompute it from `chainspec.alloc` — required, since state-actor writes synthetic state directly into RocksDB.
- `--data-storage-format=BONSAI` matches state-actor's writer layout.
- Besu has **no native post-Merge dev mode** (clique is broken post-Shanghai; the legacy `--miner-enabled` path was removed in 26.4.0). Block production must be driven via the Engine API by a mock consensus layer. The state-actor repo's `internal/engineapi/` package provides a Go-side driver; in CI, `internal/e2e_testing.StartEngineDriver` drives blocks via `engine_forkchoiceUpdated` / `getPayload` / `newPayload`.

The flags above match `host-allowlist=all` (literal `all`, not `*` — `*` would glob-expand against `/opt/besu/` in the image's entrypoint).

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

Block-number stays at 0 until a consensus layer drives `engine_forkchoiceUpdated`.

## Nethermind

Reference: `client/nethermind/e2e_test.go` (`TestE2ESuite`).

**Generate.** Nethermind uses cgo (RocksDB) — build via Docker.

```bash
docker build -f Dockerfile.nethermind -t state-actor-nethermind .
docker run --rm \
  -v /tmp/sa-neth:/data \
  state-actor-nethermind \
  ./state-actor \
  --client=nethermind --db=/data \
  --accounts=100 --contracts=15000 \
  --code-size=128 --min-slots=5 --max-slots=50 \
  --seed=42 \
  --chain-id=1337 --gas-limit=60000000 \
  --timestamp=1700000000 --extra-data=0xdeadbeef
```

**On-disk layout:**

- `/data/parity-chainspec.json` — Parity-format chainspec referenced by the boot config
- `/data/<7 RocksDB instances>` — `state`, `code`, `blocks`, `headers`, `blockNumbers`, `blockInfos`, `receipts`

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
    "NetworkingEnabled": false,
    "SynchronizationEnabled": false,
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

The full template is in `client/nethermind/e2e_test.go`'s `nethermindE2EConfigTemplate`.

**Boot.**

```bash
docker run -d \
  --name state-actor-neth \
  -v /tmp/sa-neth:/data \
  nethermind/nethermind:1.37.0 \
  --config=/data/boot.cfg \
  --log=Info
```

Like besu, Nethermind has no native post-Merge dev mode in our setup — `Merge.Enabled=true` + `TerminalTotalDifficulty=0` + the Ethash-from-genesis chainspec hand block production to MergePlugin via the Engine API. Drive blocks from an external CL mock (the state-actor repo's `internal/engineapi/`).

**Verify.**

```bash
cast chain-id --rpc-url http://<container-ip>:8545   # → 0x539
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `missing librocksdb` at build | cgo client built without the system RocksDB | Build via the per-client `Dockerfile.<client>` (see [Besu](#besu) / [Nethermind](#nethermind) / [Reth](#reth)) |
| Reth: `mmap: cannot allocate memory` | `vm.max_map_count` too low | `sudo sysctl -w vm.max_map_count=1048576` (see [Reth](#reth) operational hygiene) |
| Besu / Neth: empty `eth_blockNumber` indefinitely | No consensus layer driving the Engine API | Run a mock CL (see `internal/engineapi/`) or use `internal/e2e_testing.StartEngineDriver` ([Besu engine-API note](#besu), [Nethermind engine-API note](#nethermind)) |
| `eth_getCode` returns `0x` for a name-derived spec entity | Synthetic fill collided with the derived address | Re-run with `--accounts=0 --contracts=0` |
| Cross-client state-root divergence | Per-client `sizecal` drift, or missing canonical syscontracts | See [`ARCHITECTURE.md#cross-client-determinism`](ARCHITECTURE.md#cross-client-determinism); check `internal/clientpolicy/` calibration; ensure `syscontracts.AddCanonicalSystemContracts` ran |
| Reth: `database.version mismatch` | Boot image's reth doesn't match state-actor's pinned codec version | Pin the reth image tag; see `internal/reth/constants.go` ([Reth section](#reth)) |

## What's deliberately not in this document

No "at scale" section, no GB-tiered tuning tables, no per-client memory-profile tables, no wall-clock numbers. The library handles every size the same way; documentation that buckets by size goes stale within months and primes readers to expect different behaviour at different sizes that the code does not actually exhibit.
