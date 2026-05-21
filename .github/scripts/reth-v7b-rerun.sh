#!/bin/bash
# Re-runs the reth v7 verification + spamoor stage against the EXISTING
# v7 reth DB (133 GB at ~/work/bloatnet/data/reth), but with the new
# --dev.block-time=1s flag instead of the broken engine-driver setup.
# Designed to run in PARALLEL with the main bench (which uses port 8545)
# by binding to port 8645 + a separate container name.
#
# Does NOT regenerate the DB. The DB was last verified to still be in
# genesis state (post_spamoor_state_root == genesis_state_root in
# results/reth-result.json from the main v7 run).

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=$HOME/work/bloatnet
REPO=$HOME/state-actor
DATA=$WORK/data/reth
LOGDIR=$WORK/logs/reth-v7b
RESULTS=$WORK/results
CT=bloatnet-reth-v7b
HTTP_PORT=8645
RPC_URL=http://127.0.0.1:$HTTP_PORT
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=500

mkdir -p $LOGDIR

echo "════════════════════════════════════════════════════════════════"
echo " reth v7b — re-run with --dev.block-time=1s ($(date))"
echo "════════════════════════════════════════════════════════════════"

# Cleanup any stale container
docker rm -f $CT 2>/dev/null || true

# 1. Boot reth with --dev.block-time=1s (wall-clock ticker; no
# engine-driver + JWT plumbing needed).
echo "=== booting reth (container=$CT, data=$DATA, port=$HTTP_PORT) ==="
docker run -d --name $CT \
    --network host \
    -v $DATA:/data \
    ghcr.io/paradigmxyz/reth@sha256:e528857e5e9ebc2c6cb99f28436e70ded38ca905629f00afc98d186e27d206e0 \
    node --dev --dev.block-time=1s --debug.skip-genesis-validation \
    --datadir /data \
    --http --http.addr=127.0.0.1 --http.port=$HTTP_PORT

sleep 5

# 2. Wait for RPC
echo "=== waiting for RPC at $RPC_URL ==="
elapsed=0
while ! cast chain-id --rpc-url $RPC_URL >/dev/null 2>&1; do
    if [ $elapsed -ge 300 ]; then
        echo "RPC at $RPC_URL did not come up in 300s. Container logs:"
        docker logs --tail 100 $CT
        docker stop $CT && docker rm $CT
        exit 1
    fi
    sleep 2; elapsed=$((elapsed + 2))
done
echo "RPC ready at $RPC_URL after ${elapsed}s"

# 3. Pre-spamoor verify
echo "=== pre-spamoor verify ==="
RPC=$RPC_URL SAMPLE=500 BLOCK=latest \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-pre.log 2>&1 || true
# Strip ANSI escape sequences before counting (the script colorizes its
# PASS/FAIL markers; the v7 bench's `grep -c "^PASS"` missed the count
# because of leading \e[0;32m). Use sed to strip the CSI sequence.
PRE_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^PASS" || true)
PRE_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-pre.log | grep -c "^FAIL" || true)
PRE_PASS=${PRE_PASS:-0}
PRE_FAIL=${PRE_FAIL:-0}
echo "    pre-spamoor: $PRE_PASS passed / $PRE_FAIL failed"

# 4. Initial block tip
START_TIP=$(cast block-number --rpc-url $RPC_URL)
TARGET_TIP=$((START_TIP + SPAMOOR_TARGET_BLOCK_DELTA))
echo "=== spamoor erc20_bloater (start=$START_TIP target=$TARGET_TIP delta=$SPAMOOR_TARGET_BLOCK_DELTA) ==="

# 5. Spamoor in background, poll until target tip reached
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
echo "spamoor pid: $SPAMOOR_PID"

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
RPC=$RPC_URL SAMPLE=500 BLOCK=latest \
    bash $REPO/scripts/verify-bloatnet.sh \
    > $LOGDIR/verify-post.log 2>&1 || true
POST_PASS=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^PASS" || true)
POST_FAIL=$(sed 's/\x1b\[[0-9;]*m//g' $LOGDIR/verify-post.log | grep -c "^FAIL" || true)
POST_PASS=${POST_PASS:-0}
POST_FAIL=${POST_FAIL:-0}
echo "    post-spamoor: $POST_PASS passed / $POST_FAIL failed"

# 7. Capture
GENESIS_ROOT=$(cast block 0 --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_ROOT=$(cast block latest --rpc-url $RPC_URL --field stateRoot 2>/dev/null || echo unknown)
LATEST_BN=$(cast block-number --rpc-url $RPC_URL 2>/dev/null || echo 0)
DB_APPARENT=$(du -sh --apparent-size $DATA | cut -f1)
DB_ACTUAL=$(du -sh $DATA | cut -f1)

cat > $RESULTS/reth-v7b-result.json <<JSON
{
  "client": "reth-v7b",
  "boot_mode": "--dev --dev.block-time=1s",
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

# 8. STOP reth (preserve DB per KEEP_DBS semantics — caller decides)
docker stop $CT >/dev/null 2>&1 || true
docker rm $CT >/dev/null 2>&1 || true

echo "=== reth v7b done ($(date)) ==="
