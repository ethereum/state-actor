# client/nethermind/testdata

Boot + integration smoke artifacts for `--client=nethermind`. These pair with the
two-image build (state-actor + `nethermind/nethermind:1.39.0`) to validate that a
freshly-emitted RocksDB datadir boots cleanly and sustains real dev-mode workloads.

## Pinned target

**Nethermind `nethermind/nethermind:1.39.0`** (the released image that ships at the
time of writing). The plan originally pinned upstream/master at SHA `09bd5a2d`,
but building from that SHA tripped a Roslyn / .NET-SDK version mismatch in
`Nethermind.Analyzers`; the boot contract Nethermind enforces (`WasProcessed=true`
gate, key formats, `HeaderStore.GetBlockNumberFromBlockNumberDb` 8-byte length
check) is stable across versions, so the released image is what the smoke and
oracle tests run against.

## Files

| File | Purpose |
|---|---|
| `configs/sa-dev.json` | Nethermind runner config — `BaseDbPath=/data/db` (legacy layout, before the `db/` subdir was dropped) |
| `configs/sa-dev-v2.json` | Nethermind runner config — `BaseDbPath=/data` (current layout) |
| `chainspecs/sa-dev.json` | Parity-format chainspec: chainID 1337, NethDev engine, all EIPs through Shanghai active at genesis |
| `genesis-funded.json` | geth-format `--genesis` for state-actor; pre-funds 3 dev wallets with 1M ETH each so dev mode can sign txs |
| `send-100-txs.sh` | Sends 100 self-transfers from the first dev wallet, polls `eth_blockNumber` until block 100 |
| `spamoor-100-blocks.sh` | Runs `spamoor erc20_bloater` against the local RPC and waits until the chain advances 100 blocks (real-traffic load) |
| `validate-big-db.sh` | Boots Nethermind against a state-actor-produced datadir and runs the full validation: genesis-hash check + state-root match + dev-wallet balance + 100-tx round-trip |

## Quick reproducer (small DB)

```bash
# 1. Build the state-actor + Nethermind image
make docker-nethermind

# 2. Generate a small dev DB
make smoke-nethermind ACCOUNTS=1000 CONTRACTS=100
```

`make smoke-nethermind` runs state-actor, boots `nethermind/nethermind:1.39.0`,
and runs `validate-big-db.sh` end-to-end.

## Full pipeline (50 GB scale + spamoor `erc20_bloater`)

```bash
# 1. Build the image (one-time / on source change)
make docker-nethermind

# 2. Generate ~44 GB datadir (~28 min on Apple Silicon, single core).
#    Auto-fill emits the mainnet-shaped 20 / 10 / 70 split up to
#    --target-size; the legacy --accounts/--contracts/--max-slots/
#    --min-slots/--distribution/--code-size flags were removed in
#    #82's follow-up — those pre-#82 invocations no longer parse.
mkdir /tmp/sa-neth-big
docker run --rm \
  -v /tmp/sa-neth-big:/data \
  -v $PWD/client/nethermind/testdata:/test:ro \
  state-actor-nethermind:latest \
  --client=nethermind --db=/data \
  --target-size=44GB \
  --genesis=/test/genesis-funded.json --seed=42 --verbose

# 3. Boot Nethermind against it
docker run --rm -d --name neth-50g \
  -v $PWD/client/nethermind/testdata:/test:ro \
  -v /tmp/sa-neth-big:/data \
  -p 127.0.0.1:8545:8545 \
  nethermind/nethermind:1.39.0 \
  --config /test/configs/sa-dev-v2.json \
  --FlatDb.Enabled=true

# 4. Smoke 1 — simple self-transfers
bash client/nethermind/testdata/send-100-txs.sh

# 5. Smoke 2 — real-traffic load via spamoor erc20_bloater
#    Requires the spamoor binary on PATH (or set SPAMOOR=/abs/path).
#    Build: git clone https://github.com/ethpandaops/spamoor && cd spamoor && make
bash client/nethermind/testdata/spamoor-100-blocks.sh

# 6. Cleanup
docker stop neth-50g
```

State-actor writes Nethermind's flat-state layout, so the boot must pass
`--FlatDb.Enabled=true` — without it Nethermind selects the patricia backend
and serves empty state.

`spamoor-100-blocks.sh` deploys an ERC20 contract and runs sustained `bloatStorage()`
calls at ~16.5M gas/tx (50% of the 30M block limit). 100 blocks complete in ~1m45s
on NethDev's instant-seal cadence; each tx writes ~370 storage slots (balance +
allowance pairs). The chain head advances cleanly under load — proof the
state-actor-produced state DB is read/write-functional, not just bootable.
