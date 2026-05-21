#!/bin/bash
# reth-v8-solo.sh — gen + boot + verify + spamoor for reth alone, with
# the fixed state-actor:reth image (commit 98aba19, drops AppendDup on
# StoragesTrie). Runs in PARALLEL with the main bench's besu phase by
# binding to port 8645 + a separate container name. Outputs result.json
# to ~/work/bloatnet/results/reth-v8-result.json so the comparison view
# can pick it up alongside the besu result.

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=$HOME/work/bloatnet
REPO=$HOME/state-actor
DATA=$WORK/data/reth
LOGDIR=$WORK/logs/reth-v8
RESULTS=$WORK/results
CT=bloatnet-reth-v8
HTTP_PORT=8645
P2P_PORT=30403   # non-default to avoid clashes with neth/besu's default 30303
RPC_URL=http://127.0.0.1:$HTTP_PORT
SPEC=$WORK/spec-bloatnet-100gb.yaml
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=500

mkdir -p $LOGDIR $DATA

echo "════════════════════════════════════════════════════════════════"
echo " reth v8 (parallel) — fixed StoragesTrie writes ($(date))"
echo "════════════════════════════════════════════════════════════════"

# Defensive: kill stale containers from prior debugging sessions that
# could be holding ports we need (30303 default p2p, 8551 engine, etc.).
docker rm -f $CT neth-probe besu-quick-probe bloatnet-besu-reverify 2>/dev/null || true

# 1. Generate (with the new image — trie persistence + syscontracts).
echo "=== state-actor generation (writing to $DATA) ==="
/usr/bin/time -v docker run --rm \
    -v $SPEC:/spec.yaml \
    -v $DATA:/data \
    state-actor:reth \
    --client=reth \
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

# 2. Boot reth with --dev.block-time=1s (no engine-driver, no JWT —
# the local-miner wall-clock ticker handles block production).
echo "=== booting reth (container=$CT, port=$HTTP_PORT) ==="
docker run -d --name $CT \
    --network host \
    -v $DATA:/data \
    ghcr.io/paradigmxyz/reth@sha256:e528857e5e9ebc2c6cb99f28436e70ded38ca905629f00afc98d186e27d206e0 \
    node --dev --dev.block-time=1s --debug.skip-genesis-validation \
    --chain /data/chainspec.json \
    --datadir /data \
    --port $P2P_PORT \
    --discovery.port $P2P_PORT \
    --http --http.addr=127.0.0.1 --http.port=$HTTP_PORT --http.api=eth,net,web3,txpool

# 3. Wait for RPC
elapsed=0
while ! cast chain-id --rpc-url $RPC_URL >/dev/null 2>&1; do
    if [ $elapsed -ge 300 ]; then
        echo "RPC at $RPC_URL did not come up; container logs:"
        docker logs --tail 100 $CT
        docker stop $CT 2>/dev/null; docker rm $CT 2>/dev/null
        exit 1
    fi
    sleep 2; elapsed=$((elapsed + 2))
done
echo "RPC ready at $RPC_URL after ${elapsed}s"

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

# 6. Post-spamoor verify (with chain-advance + BEACON_ROOTS gates)
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

cat > $RESULTS/reth-v8-result.json <<JSON
{
  "client": "reth-v8",
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

# 8. STOP + preserve DB
docker stop $CT >/dev/null 2>&1 || true
docker rm $CT >/dev/null 2>&1 || true

echo "=== reth v8 done ($(date)) ==="
