#!/bin/bash
# nethermind-v8-solo.sh — gen + boot + verify + spamoor for nethermind
# alone, with the syscontracts-aware state-actor:nethermind image (post
# commit 57d0b46) and the CLI fixes:
#
#   1. --Init.ChainSpecPath=/data/parity-chainspec.json (was MISSING in
#      run-bloatnet.sh — caused mainnet-chainspec fallback)
#   2. --JsonRpc.JwtSecretFile dropped (was passed empty in run-bloatnet.sh
#      — caused nethermind to abort with "Required argument missing")
#
# Nethermind has no native --dev mode wall-clock miner. We drive blocks
# via engine-driver (from $WORK/bin/engine-driver), same pattern besu uses.
#
# Outputs $WORK/results/nethermind-v8-result.json.

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=$HOME/work/bloatnet
REPO=$HOME/state-actor
DATA=$WORK/data/nethermind
LOGDIR=$WORK/logs/nethermind-v8
RESULTS=$WORK/results
CT=bloatnet-nethermind-v8
HTTP_PORT=8545
ENGINE_PORT=8551
P2P_PORT=30503   # non-default to avoid clashes with reth's 30403 / besu's 30303
RPC_URL=http://127.0.0.1:$HTTP_PORT
ENGINE_URL=http://127.0.0.1:$ENGINE_PORT
SPEC=$WORK/spec-bloatnet-100gb.yaml
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=500
ENGINE_DRIVER=$WORK/bin/engine-driver

mkdir -p $LOGDIR $DATA $RESULTS

echo "════════════════════════════════════════════════════════════════"
echo " nethermind v8 — syscontracts + CLI fixes ($(date))"
echo "════════════════════════════════════════════════════════════════"

# Defensive: kill any stale container that might be holding our ports.
docker rm -f $CT neth-probe besu-quick-probe bloatnet-besu-reverify 2>/dev/null || true
pkill -f engine-driver 2>/dev/null || true

# 1. Generate (with the syscontracts-aware image).
echo "=== state-actor generation (writing to $DATA) ==="
/usr/bin/time -v docker run --rm \
    -v $SPEC:/spec.yaml \
    -v $DATA:/data \
    state-actor:nethermind \
    --client=nethermind \
    --db=/data \
    --spec=/spec.yaml \
    --seed=42 \
    --fork=osaka \
    --chain-id=1337 \
    --gas-limit=60000000 \
    --target-size=100GB \
    --accounts=0 \
    --contracts=0 \
    --verbose \
    > $LOGDIR/gen.log 2>&1
gen_status=$?
if [ $gen_status -ne 0 ]; then
    echo "state-actor generation FAILED (exit $gen_status). See $LOGDIR/gen.log"
    exit 1
fi
echo "=== generation done; DB size: $(du -sh $DATA | cut -f1) ==="

# Capture the writer-claimed genesis state root for cross-check.
WRITER_STATE_ROOT=$(grep -oE 'state root\s*=\s*0x[0-9a-fA-F]+' $LOGDIR/gen.log | tail -1 | awk -F= '{print $2}' | tr -d ' ')
WRITER_GENESIS_HASH=$(grep -oE 'genesis hash\s*=\s*0x[0-9a-fA-F]+' $LOGDIR/gen.log | tail -1 | awk -F= '{print $2}' | tr -d ' ')
echo "    writer claims: stateRoot=$WRITER_STATE_ROOT genesisHash=$WRITER_GENESIS_HASH"

# 2. Boot nethermind with --Init.ChainSpecPath + sync/networking disabled.
#
# Sync.{Networking,Synchronization}Enabled=false + Init.PeerManagerEnabled=false
# match the CI boot config at client/nethermind/e2e_test.go:48-83. Without
# these, Nethermind's GenesisBuilder re-derives genesis from the chainspec's
# empty accounts map (the chainspec writer doesn't emit stateRoot — tracked
# as a separate latent bug), overwriting state-actor's on-disk genesis with
# the empty-MPT-root one. With sync disabled, GenesisBuilder skips that
# path and the on-disk genesis state survives.
echo "=== booting nethermind (container=$CT, port=$HTTP_PORT, p2p=$P2P_PORT) ==="
docker run -d --name $CT \
    --network host \
    -v $DATA:/data \
    nethermind/nethermind:1.37.0 \
    --datadir /data \
    --Init.ChainSpecPath=/data/parity-chainspec.json \
    --Init.BaseDbPath=/data \
    --Init.PeerManagerEnabled=false \
    --Init.DiscoveryEnabled=false \
    --Sync.NetworkingEnabled=false \
    --Sync.SynchronizationEnabled=false \
    --Sync.PivotNumber=0 \
    --Network.P2PPort=$P2P_PORT \
    --Network.DiscoveryPort=$P2P_PORT \
    --Network.ActivePeersMaxCount=0 \
    --JsonRpc.Enabled=true --JsonRpc.Host=127.0.0.1 --JsonRpc.Port=$HTTP_PORT \
    --JsonRpc.EngineHost=127.0.0.1 --JsonRpc.EnginePort=$ENGINE_PORT \
    --JsonRpc.UnsecureDevNoRpcAuthentication=true \
    --Merge.Enabled=true --Merge.TerminalTotalDifficulty=0

# 3. Wait for RPC. .NET clients can take 30-60s on a cold start with 105GB
# DB; bump the timeout generously.
echo "=== waiting for RPC at $RPC_URL (cold-start can be slow) ==="
elapsed=0
while ! cast chain-id --rpc-url $RPC_URL >/dev/null 2>&1; do
    if [ $elapsed -ge 600 ]; then
        echo "RPC at $RPC_URL did not come up in 600s; container logs:"
        docker logs --tail 80 $CT
        docker stop $CT 2>/dev/null; docker rm $CT 2>/dev/null
        exit 1
    fi
    sleep 5; elapsed=$((elapsed + 5))
done
echo "RPC ready at $RPC_URL after ${elapsed}s"

# Cross-check the live genesis hash vs what state-actor wrote.
LIVE_GENESIS_HASH=$(cast block 0 --rpc-url $RPC_URL --field hash 2>/dev/null || echo unknown)
LIVE_STATE_ROOT=$(cast block 0 --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
echo "    live block 0:   stateRoot=$LIVE_STATE_ROOT hash=$LIVE_GENESIS_HASH"

# 4. Start engine-driver — nethermind has no native local-miner ticker;
# blocks come via engine_forkchoiceUpdated + engine_newPayload over 8551.
echo "=== starting engine-driver ==="
nohup $ENGINE_DRIVER \
    -engine $ENGINE_URL \
    -eth $RPC_URL \
    -fork osaka \
    -block-time 1s \
    > $LOGDIR/engine-driver.log 2>&1 &
ENGINE_PID=$!
echo $ENGINE_PID > $LOGDIR/engine-driver.pid

# 5. Pre-spamoor verify
echo "=== pre-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-pre.log 2>&1 || true
PRE_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^PASS" || true)
PRE_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^FAIL" || true)
PRE_PASS=${PRE_PASS:-0}; PRE_FAIL=${PRE_FAIL:-0}
echo "    pre-spamoor: $PRE_PASS passed / $PRE_FAIL failed"

# 6. Spamoor 500 blocks
START_TIP=$(cast block-number --rpc-url $RPC_URL)
TARGET_TIP=$((START_TIP + SPAMOOR_TARGET_BLOCK_DELTA))
echo "=== spamoor erc20_bloater (start=$START_TIP target=$TARGET_TIP) ==="

spamoor erc20_bloater \
    --rpchost $RPC_URL \
    --privkey $SPAMOOR_PRIVKEY \
    --seed 12345 \
    --target-gb 999 \
    --target-gas-ratio 0.1 \
    --wallet-count 5 \
    --slot-duration 1s \
    > $LOGDIR/spamoor.log 2>&1 &
SPAMOOR_PID=$!

POLL_DEADLINE=$(( $(date +%s) + 1800 ))
while [ $(date +%s) -lt $POLL_DEADLINE ]; do
    CUR=$(cast block-number --rpc-url $RPC_URL 2>/dev/null || echo $START_TIP)
    if [ $CUR -ge $TARGET_TIP ]; then
        echo "    reached target tip ($CUR >= $TARGET_TIP)"
        break
    fi
    if ! kill -0 $SPAMOOR_PID 2>/dev/null; then
        echo "    spamoor exited unexpectedly at tip=$CUR"
        break
    fi
    sleep 5
done

kill -TERM $SPAMOOR_PID 2>/dev/null || true
for i in $(seq 1 10); do
    if ! kill -0 $SPAMOOR_PID 2>/dev/null; then break; fi
    sleep 1
done
kill -KILL $SPAMOOR_PID 2>/dev/null || true
wait $SPAMOOR_PID 2>/dev/null || true

# 7. Post-spamoor verify
echo "=== post-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest CHECK_CHAIN_ADVANCED=1 \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-post.log 2>&1 || true
POST_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^PASS" || true)
POST_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^FAIL" || true)
POST_PASS=${POST_PASS:-0}; POST_FAIL=${POST_FAIL:-0}
echo "    post-spamoor: $POST_PASS passed / $POST_FAIL failed"

# 8. Capture result
GENESIS_ROOT=$(cast block 0 --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_ROOT=$(cast block latest --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_BN=$(cast block-number --rpc-url $RPC_URL 2>/dev/null || echo 0)
DB_APPARENT=$(du -sh --apparent-size $DATA | cut -f1)
DB_ACTUAL=$(du -sh $DATA | cut -f1)

cat > $RESULTS/nethermind-v8-result.json <<JSON
{
  "client": "nethermind-v8",
  "boot_mode": "--Init.ChainSpecPath + engine-driver block-time=1s",
  "writer_genesis_hash": "$WRITER_GENESIS_HASH",
  "writer_state_root": "$WRITER_STATE_ROOT",
  "genesis_state_root": "$GENESIS_ROOT",
  "post_spamoor_state_root": "$LATEST_ROOT",
  "post_spamoor_block_number": $LATEST_BN,
  "pre_verify_pass": $PRE_PASS,
  "pre_verify_fail": $PRE_FAIL,
  "post_verify_pass": $POST_PASS,
  "post_verify_fail": $POST_FAIL,
  "db_size_apparent": "$DB_APPARENT",
  "db_size_actual": "$DB_ACTUAL"
}
JSON
echo "    genesis_root: $GENESIS_ROOT"
echo "    latest_root:  $LATEST_ROOT"
echo "    latest_bn:    $LATEST_BN"
echo "    db_size:      apparent=$DB_APPARENT actual=$DB_ACTUAL"

# 9. STOP + preserve DB
kill $ENGINE_PID 2>/dev/null || true
rm -f $LOGDIR/engine-driver.pid
docker stop $CT >/dev/null 2>&1 || true
docker rm $CT >/dev/null 2>&1 || true

echo "=== nethermind v8 done ($(date)) ==="
