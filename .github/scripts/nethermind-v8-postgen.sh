#!/bin/bash
# nethermind-v8-postgen.sh — boot + verify + spamoor against an
# ALREADY-GENERATED nethermind DB at $WORK/data/nethermind.
#
# Same boot / engine-driver / verify / spamoor sequence as
# nethermind-v8-solo.sh but skips the 90-min state-actor generation step.
# Use this when the gen DB is intact (post a Phase 0 worker-pool gen) and
# we only need to (re-)verify the post-boot behavior.
#
# Refuses to run if the DB directory is missing or empty.
#
# Outputs $WORK/results/nethermind-v8-postgen-result.json so it doesn't
# clobber the original solo-run result.

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=$HOME/work/bloatnet
REPO=$HOME/state-actor
DATA=$WORK/data/nethermind
LOGDIR=$WORK/logs/nethermind-v8-postgen
RESULTS=$WORK/results
CT=bloatnet-nethermind-v8-pg
HTTP_PORT=8545
ENGINE_PORT=8551
P2P_PORT=30503
RPC_URL=http://127.0.0.1:$HTTP_PORT
ENGINE_URL=http://127.0.0.1:$ENGINE_PORT
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=500
ENGINE_DRIVER=$WORK/bin/engine-driver

mkdir -p $LOGDIR $RESULTS

echo "════════════════════════════════════════════════════════════════"
echo " nethermind v8 POSTGEN — boot + verify against existing DB ($(date))"
echo "════════════════════════════════════════════════════════════════"

# Sanity: DB must exist and not be empty.
if [ ! -d "$DATA" ] || [ -z "$(ls -A $DATA 2>/dev/null)" ]; then
    echo "ERROR: $DATA missing or empty. Run nethermind-v8-solo.sh to gen first."
    exit 1
fi
echo "DB present: $(du -sh $DATA | cut -f1) at $DATA"

# Defensive: kill any stale container holding our ports + any old engine-driver.
docker rm -f $CT bloatnet-nethermind-v8 neth-probe 2>/dev/null || true
pkill -f engine-driver 2>/dev/null || true

# Sanity check the engine-driver binary
if [ ! -x "$ENGINE_DRIVER" ]; then
    echo "ERROR: engine-driver not executable at $ENGINE_DRIVER"
    exit 1
fi

# Boot nethermind.
#
# Sync/PeerManager disablement matches the CI e2e boot config at
# client/nethermind/e2e_test.go:48-83. Without these flags Nethermind's
# GenesisBuilder re-derives the genesis from the chainspec (which doesn't
# emit stateRoot, a separate latent bug) and overwrites the on-disk
# state-actor genesis with the empty-MPT-root one.
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

# Wait for RPC. Cold start with 105 GB DB can be slow.
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

LIVE_GENESIS_HASH=$(cast block 0 --rpc-url $RPC_URL --field hash 2>/dev/null || echo unknown)
LIVE_STATE_ROOT=$(cast block 0 --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
echo "    live block 0:   stateRoot=$LIVE_STATE_ROOT hash=$LIVE_GENESIS_HASH"

# Start engine-driver.
echo "=== starting engine-driver ==="
nohup $ENGINE_DRIVER \
    -engine $ENGINE_URL \
    -eth $RPC_URL \
    -fork osaka \
    -block-time 1s \
    > $LOGDIR/engine-driver.log 2>&1 &
ENGINE_PID=$!
echo $ENGINE_PID > $LOGDIR/engine-driver.pid

# Pre-spamoor verify.
echo "=== pre-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-pre.log 2>&1 || true
PRE_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^PASS" || true)
PRE_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^FAIL" || true)
PRE_PASS=${PRE_PASS:-0}; PRE_FAIL=${PRE_FAIL:-0}
echo "    pre-spamoor: $PRE_PASS passed / $PRE_FAIL failed"

# Spamoor 500 blocks.
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

# Post-spamoor verify.
echo "=== post-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest CHECK_CHAIN_ADVANCED=1 \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-post.log 2>&1 || true
POST_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^PASS" || true)
POST_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^FAIL" || true)
POST_PASS=${POST_PASS:-0}; POST_FAIL=${POST_FAIL:-0}
echo "    post-spamoor: $POST_PASS passed / $POST_FAIL failed"

# Capture result.
GENESIS_ROOT=$(cast block 0 --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_ROOT=$(cast block latest --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_BN=$(cast block-number --rpc-url $RPC_URL 2>/dev/null || echo 0)
DB_APPARENT=$(du -sh --apparent-size $DATA | cut -f1)
DB_ACTUAL=$(du -sh $DATA | cut -f1)

cat > $RESULTS/nethermind-v8-postgen-result.json <<JSON
{
  "client": "nethermind-v8-postgen",
  "boot_mode": "--Init.ChainSpecPath + engine-driver block-time=1s (DB reused)",
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

# STOP + preserve DB.
kill $ENGINE_PID 2>/dev/null || true
rm -f $LOGDIR/engine-driver.pid
docker stop $CT >/dev/null 2>&1 || true
docker rm $CT >/dev/null 2>&1 || true

echo "=== nethermind v8 postgen done ($(date)) ==="
