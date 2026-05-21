#!/bin/bash
# besu-v8-reboot.sh — boot the existing v8 besu DB (gen'd at 150 GB
# earlier in this bench) with the new --genesis-state-hash-cache-enabled
# flag that resolves the "Supplied genesis block does not match chain
# data" mismatch. Then run pre-spamoor verify + spamoor 500 blocks +
# post-spamoor verify. No regen needed — the DB is intact at
# ~/work/bloatnet/data/besu.

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=$HOME/work/bloatnet
REPO=$HOME/state-actor
DATA=$WORK/data/besu
LOGDIR=$WORK/logs/besu-v8b
RESULTS=$WORK/results
CT=bloatnet-besu-v8b
HTTP_PORT=8545
RPC_URL=http://127.0.0.1:$HTTP_PORT
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=500

mkdir -p $LOGDIR

ENGINE_DRIVER=$WORK/bin/engine-driver

echo "════════════════════════════════════════════════════════════════"
echo " besu v8b — reboot with --genesis-state-hash-cache-enabled ($(date))"
echo "════════════════════════════════════════════════════════════════"

# Clean stale container + any engine-driver from the failed v8 run
docker rm -f $CT 2>/dev/null || true
pkill -f engine-driver 2>/dev/null || true

# 1. Boot besu with the chainspec-cache-enabled flag.
echo "=== booting besu (container=$CT, port=$HTTP_PORT, cache-enabled=YES) ==="
docker run -d --name $CT \
    --network host \
    -v $DATA:/data \
    hyperledger/besu:25.11.0 \
    --genesis-file=/data/besu-chainspec.json \
    --genesis-state-hash-cache-enabled \
    --data-storage-format=BONSAI \
    --data-path=/data \
    --rpc-http-enabled --rpc-http-host=127.0.0.1 --rpc-http-port=$HTTP_PORT \
    --rpc-http-api=ETH,NET,WEB3,TXPOOL,DEBUG \
    --host-allowlist=all \
    --engine-rpc-enabled --engine-host-allowlist=all \
    --engine-rpc-port=8551 \
    --engine-jwt-disabled

# 2. Wait for RPC. Besu boot on 150 GB DB can take a few minutes.
echo "=== waiting for RPC at $RPC_URL (besu boot is slow on big DBs) ==="
elapsed=0
while ! cast chain-id --rpc-url $RPC_URL >/dev/null 2>&1; do
    if [ $elapsed -ge 900 ]; then
        echo "RPC at $RPC_URL did not come up in 900s; container logs:"
        docker logs --tail 100 $CT
        docker stop $CT 2>/dev/null; docker rm $CT 2>/dev/null
        exit 1
    fi
    sleep 5; elapsed=$((elapsed + 5))
done
echo "RPC ready at $RPC_URL after ${elapsed}s"

# 3. Start engine-driver (besu has no native dev-mode block production)
echo "=== starting engine-driver for besu ==="
nohup $ENGINE_DRIVER \
    -engine http://127.0.0.1:8551 \
    -eth $RPC_URL \
    -fork osaka \
    -block-time 1s \
    > $LOGDIR/engine-driver.log 2>&1 &
ENGINE_PID=$!

# 4. Pre-spamoor verify
echo "=== pre-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-pre.log 2>&1 || true
PRE_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^PASS" || true)
PRE_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^FAIL" || true)
PRE_PASS=${PRE_PASS:-0}; PRE_FAIL=${PRE_FAIL:-0}
echo "    pre-spamoor: $PRE_PASS passed / $PRE_FAIL failed"

# 5. Spamoor 500 blocks
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

# 6. Post-spamoor verify
echo "=== post-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest CHECK_CHAIN_ADVANCED=1 \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-post.log 2>&1 || true
POST_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^PASS" || true)
POST_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^FAIL" || true)
POST_PASS=${POST_PASS:-0}; POST_FAIL=${POST_FAIL:-0}
echo "    post-spamoor: $POST_PASS passed / $POST_FAIL failed"

# 7. Capture
GENESIS_ROOT=$(cast block 0 --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_ROOT=$(cast block latest --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_BN=$(cast block-number --rpc-url $RPC_URL 2>/dev/null || echo 0)
DB_APPARENT=$(du -sh --apparent-size $DATA | cut -f1)
DB_ACTUAL=$(du -sh $DATA | cut -f1)

cat > $RESULTS/besu-v8b-result.json <<JSON
{
  "client": "besu-v8b",
  "boot_mode": "--genesis-state-hash-cache-enabled + engine-driver",
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

# 8. STOP + preserve DB
kill $ENGINE_PID 2>/dev/null || true
docker stop $CT >/dev/null 2>&1 || true
docker rm $CT >/dev/null 2>&1 || true

echo "=== besu v8b done ($(date)) ==="
