#!/bin/bash
# run-bloatnet.sh — master orchestrator for the 100 GB bloatnet
# benchmark. Runs state-actor → boot client → RPC verify → spamoor 500
# blocks for each client in CLIENTS (default: all 4, serial).
#
# Prerequisites (must already be on PATH):
#   docker, cast (foundry), spamoor, /usr/local/go/bin/go
#
# Required env / args:
#   WORK=$HOME/work/bloatnet           # work root
#   STATE_ACTOR_REPO=$HOME/state-actor # repo containing scripts/
#   CLIENTS="geth reth nethermind besu"

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=${WORK:-$HOME/work/bloatnet}
REPO=${STATE_ACTOR_REPO:-$HOME/state-actor}
CLIENTS=${CLIENTS:-geth reth nethermind besu}
SPEC=$WORK/spec-bloatnet-100gb.yaml
SEED=${SEED:-42}
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=${SPAMOOR_BLOCKS:-500}
# ARCHIVE=1 opts state-actor + geth into archive mode. Default off →
# full mode (smaller reth DB; geth runtime unaffected at genesis but
# the bench loop boots it with --gcmode=archive when set, matching how
# state-actor wrote the anchor metadata).
ARCHIVE=${ARCHIVE:-}

mkdir -p $WORK/{logs,data,results}

# ── Sanity: prereqs ───────────────────────────────────────────────────
for cmd in docker cast spamoor go; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "missing: $cmd" >&2; exit 1
    fi
done

# ── Phase 2: generate the spec YAML if not already done ──────────────
if [ ! -s "$SPEC" ]; then
    echo "=== generating spec → $SPEC (seed=4242) ==="
    cd $REPO
    go run ./.github/scripts/gen-bloatnet-spec -out $SPEC -seed 4242
fi
echo "=== spec: $(ls -lh $SPEC | awk '{print $5}') ==="

# ── Phase 2.5: build the engine-driver binary ─────────────────────────
ENGINE_DRIVER=$WORK/bin/engine-driver
mkdir -p $WORK/bin
if [ ! -x "$ENGINE_DRIVER" ] || [ "$REPO/.github/scripts/engine-driver/main.go" -nt "$ENGINE_DRIVER" ]; then
    echo "=== building engine-driver ==="
    cd $REPO
    go build -o $ENGINE_DRIVER ./.github/scripts/engine-driver/
fi

# ── Phase 2.6: per-run engine-API JWT secret ──────────────────────────
# Reth requires --authrpc.jwtsecret pointing at a 64-hex-char file (raw
# 32-byte HMAC-SHA256 key). The same file is passed to engine-driver via
# -jwt-secret so the CL mock can sign its engine_* calls. Besu/nethermind
# boot with --engine-jwt-disabled and ignore the file.
JWT_HEX=$WORK/jwt.hex
if [ ! -s "$JWT_HEX" ]; then
    echo "=== generating engine-API JWT secret → $JWT_HEX ==="
    openssl rand -hex 32 > $JWT_HEX
    chmod 0644 $JWT_HEX  # docker mounts it as ro; readable by the EL
fi

# ── Helper: wait for JSON-RPC ready ───────────────────────────────────
wait_for_rpc() {
    local url=$1 timeout=${2:-300}
    local elapsed=0
    while ! cast chain-id --rpc-url $url >/dev/null 2>&1; do
        if [ $elapsed -ge $timeout ]; then
            echo "RPC at $url did not come up in ${timeout}s" >&2
            return 1
        fi
        sleep 2; elapsed=$((elapsed + 2))
    done
    echo "RPC ready at $url after ${elapsed}s"
}

# ── Per-client boot ───────────────────────────────────────────────────
boot_client() {
    local client=$1 data=$2 ct=$3
    echo "=== booting $client (container=$ct, data=$data) ==="
    case $client in
        geth)
            local gc_args=""
            if [ -n "$ARCHIVE" ]; then
                gc_args="--gcmode=archive"
            fi
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                ethereum/client-go:v1.17.2 \
                --datadir /data --dev --dev.period=1 --dev.gaslimit=60000000 \
                --db.engine=pebble \
                $gc_args \
                --http --http.addr=127.0.0.1 --http.port=8545 \
                --http.api=eth,net,web3,txpool,debug
            ;;
        besu)
            # --genesis-state-hash-cache-enabled tells besu to TRUST the
            # DB-stored stateRoot rather than recompute from chainspec.alloc.
            # state-actor writes alloc={} (the real state lives in BONSAI
            # tables); without this flag besu recomputes a different root
            # and rejects boot with "Supplied genesis block does not match
            # chain data stored". See client/besu/chainspec.go:30-37.
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                hyperledger/besu:25.11.0 \
                --genesis-file=/data/besu-chainspec.json \
                --genesis-state-hash-cache-enabled \
                --data-storage-format=BONSAI \
                --data-path=/data \
                --rpc-http-enabled --rpc-http-host=127.0.0.1 --rpc-http-port=8545 \
                --rpc-http-api=ETH,NET,WEB3,TXPOOL,DEBUG \
                --host-allowlist=all \
                --engine-rpc-enabled --engine-host-allowlist=all \
                --engine-rpc-port=8551 \
                --engine-jwt-disabled
            ;;
        nethermind)
            # CLI requirements (verified against nethermind/nethermind:1.37.0 help):
            # 1. --Init.ChainSpecPath is REQUIRED — without it nethermind boots
            #    with the default foundation (mainnet) chainspec and rejects
            #    our DB's genesis as mismatched. state-actor writes the parity
            #    chainspec next to the DB.
            # 2. --JsonRpc.JwtSecretFile must NOT be passed with an empty
            #    value — nethermind errors "Required argument missing for
            #    option" and dumps help text. Default (`null`) = no JWT, which
            #    is what we want since engine-driver uses --engine-jwt-disabled.
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                nethermind/nethermind:1.37.0 \
                --datadir /data \
                --Init.ChainSpecPath=/data/parity-chainspec.json \
                --JsonRpc.Enabled=true --JsonRpc.Host=127.0.0.1 --JsonRpc.Port=8545 \
                --JsonRpc.EngineHost=127.0.0.1 --JsonRpc.EnginePort=8551 \
                --Merge.Enabled=true --Merge.TerminalTotalDifficulty=0
            ;;
        reth)
            # --dev.block-time=1s drives the local-miner via a wall-clock
            # ticker (tokio::time::interval_at), independent of the tx pool.
            # This is the direct equivalent of geth's --dev.period=1.
            # Without it, --dev uses MiningMode::Instant which only mines
            # when a tx hits the pool — deadlocking with spamoor's funding
            # phase. Source: crates/engine/local/src/miner.rs:113-118,
            # crates/node/core/src/node_config.rs:586-595. Lets us drop
            # the engine-driver + JWT plumbing entirely for reth.
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                ghcr.io/paradigmxyz/reth@sha256:e528857e5e9ebc2c6cb99f28436e70ded38ca905629f00afc98d186e27d206e0 \
                node --dev --dev.block-time=1s --debug.skip-genesis-validation \
                --chain /data/chainspec.json \
                --datadir /data \
                --http --http.addr=127.0.0.1 --http.port=8545 --http.api=eth,net,web3,txpool
            ;;
        *)
            echo "unknown client: $client" >&2; return 1 ;;
    esac
    sleep 3   # let the container start before checking liveness
}

# ── Per-client: start engine driver if not auto-mining ────────────────
start_engine_driver_if_needed() {
    local client=$1 logdir=$2
    # geth + reth self-mine via --dev.period / --dev.block-time and need
    # no external CL. besu + nethermind have no equivalent wall-clock
    # local-miner, so engine-driver drives FCU + newPayload over engine
    # API on port 8551 with --engine-jwt-disabled (no JWT header sent).
    case $client in
        besu|nethermind)
            echo "=== starting engine-driver for $client ==="
            nohup $ENGINE_DRIVER \
                -engine http://127.0.0.1:8551 \
                -eth http://127.0.0.1:8545 \
                -fork osaka \
                -block-time 1s \
                > $logdir/engine-driver.log 2>&1 &
            echo "engine-driver pid: $!" > $logdir/engine-driver.pid
            ;;
        geth|reth)
            # Self-mining via --dev flag; no engine driver needed.
            ;;
    esac
}

stop_engine_driver() {
    local logdir=$1
    if [ -f $logdir/engine-driver.pid ]; then
        local pid=$(awk '{print $NF}' $logdir/engine-driver.pid)
        kill $pid 2>/dev/null || true
        rm -f $logdir/engine-driver.pid
    fi
}

# ── Per-client run ────────────────────────────────────────────────────
run_one_client() {
    local client=$1
    local data=$WORK/data/$client
    local logdir=$WORK/logs/$client
    local results=$WORK/results
    local ct="bloatnet-$client"

    mkdir -p $data $logdir $results

    echo
    echo "════════════════════════════════════════════════════════════════"
    echo " $client  ($(date))"
    echo "════════════════════════════════════════════════════════════════"

    # 1. Generate the DB. Per-client db path:
    #   - geth needs <datadir>/geth/chaindata under --datadir.
    #   - besu/neth/reth take the bare datadir.
    local db_path="/data"
    if [ "$client" = "geth" ]; then
        db_path="/data/geth/chaindata"
    fi

    # Per-client archive flag plumbing. --archive is supported on geth
    # and reth; besu/nethermind reject it at CLI parse.
    local archive_arg=""
    if [ -n "$ARCHIVE" ]; then
        case $client in
            geth|reth) archive_arg="--archive" ;;
        esac
    fi

    echo "=== state-actor generation (writing to $data, container path $db_path${archive_arg:+, archive=on}) ==="
    /usr/bin/time -v docker run --rm \
        -v $SPEC:/spec.yaml \
        -v $data:/data \
        state-actor:$client \
        --client=$client \
        --db=$db_path \
        --spec=/spec.yaml \
        --seed=$SEED \
        --fork=osaka \
        --chain-id=1337 \
        --gas-limit=60000000 \
        --target-size=100GB \
        --accounts=0 \
        --contracts=0 \
        $archive_arg \
        --verbose \
        > $logdir/gen.log 2>&1
    local gen_status=$?
    if [ $gen_status -ne 0 ]; then
        echo "state-actor generation FAILED (exit $gen_status). See $logdir/gen.log"
        return 1
    fi
    echo "=== generation done; DB size: $(du -sh $data | cut -f1) ==="

    # 2. Boot the client
    docker rm -f $ct 2>/dev/null || true
    boot_client $client $data $ct
    start_engine_driver_if_needed $client $logdir

    # 3. Wait for RPC. JVM (besu) and .NET (nethermind) clients need
    # significantly longer to open a 100-150 GB DB on first boot:
    # block-cache warm-up, lazy SST opens, etc. Geth/reth are seconds.
    local rpc_timeout=300
    case $client in
        besu|nethermind) rpc_timeout=900 ;;
    esac
    if ! wait_for_rpc "http://127.0.0.1:8545" $rpc_timeout; then
        echo "RPC never came up; container logs:"; docker logs --tail 50 $ct
        docker stop $ct && docker rm $ct
        stop_engine_driver $logdir
        return 1
    fi

    # 4. Pre-spamoor verify (block=latest, samples=500)
    echo "=== pre-spamoor verify ==="
    RPC=http://127.0.0.1:8545 SAMPLE=500 BLOCK=latest \
        bash $REPO/scripts/verify-bloatnet.sh \
        > $logdir/verify-pre.log 2>&1 || true
    # verify-bloatnet.sh colorizes its PASS/FAIL markers via ANSI CSI
    # sequences; strip them before counting so `grep -c "^PASS"` matches
    # (without strip, every count was 0). Pipe through `|| true` rather
    # than `|| echo 0` — the latter appended a stray "0\n" newline that
    # broke the result.json heredoc downstream.
    local pre_pass=$(sed 's/\x1b\[[0-9;]*m//g' $logdir/verify-pre.log | grep -c "^PASS" 2>/dev/null || true)
    local pre_fail=$(sed 's/\x1b\[[0-9;]*m//g' $logdir/verify-pre.log | grep -c "^FAIL" 2>/dev/null || true)
    pre_pass=${pre_pass:-0}
    pre_fail=${pre_fail:-0}
    echo "    pre-spamoor:  $pre_pass passed / $pre_fail failed"

    # 5. spamoor erc20_bloater. Spamoor itself has no block-count cap;
    # we wrap it: launch in background, poll eth_blockNumber, SIGTERM
    # when tip advances by SPAMOOR_TARGET_BLOCK_DELTA. Mirrors what
    # internal/e2e_testing/spamoor.go::SpamoorRun does for the CI suite.
    local start_tip=$(cast block-number --rpc-url http://127.0.0.1:8545)
    local target_tip=$((start_tip + SPAMOOR_TARGET_BLOCK_DELTA))
    echo "=== spamoor erc20_bloater (start=$start_tip target=$target_tip delta=$SPAMOOR_TARGET_BLOCK_DELTA) ==="

    spamoor erc20_bloater \
        --rpchost http://127.0.0.1:8545 \
        --privkey $SPAMOOR_PRIVKEY \
        --seed 12345 \
        --target-gb 999 \
        --target-gas-ratio 0.1 \
        --wallet-count 5 \
        --slot-duration 1s \
        > $logdir/spamoor.log 2>&1 &
    local spamoor_pid=$!

    # Poll until target tip reached or 30 min timeout
    local poll_deadline=$(( $(date +%s) + 1800 ))
    while [ $(date +%s) -lt $poll_deadline ]; do
        local cur=$(cast block-number --rpc-url http://127.0.0.1:8545 2>/dev/null || echo $start_tip)
        if [ $cur -ge $target_tip ]; then
            echo "    reached target tip ($cur >= $target_tip)"
            break
        fi
        if ! kill -0 $spamoor_pid 2>/dev/null; then
            echo "    spamoor exited unexpectedly at tip=$cur"
            break
        fi
        sleep 5
    done

    # Stop spamoor (SIGTERM, fallback SIGKILL after 10s)
    kill -TERM $spamoor_pid 2>/dev/null || true
    for i in $(seq 1 10); do
        if ! kill -0 $spamoor_pid 2>/dev/null; then break; fi
        sleep 1
    done
    kill -KILL $spamoor_pid 2>/dev/null || true
    wait $spamoor_pid 2>/dev/null || true

    # 6. Post-spamoor sanity verify. CHECK_CHAIN_ADVANCED=1 enables the
    # post-block-1 gates (chain-advance + BEACON_ROOTS ring-buffer);
    # the pre-spamoor invocation above doesn't set it so block=0 is
    # tolerated at genesis.
    echo "=== post-spamoor verify ==="
    RPC=http://127.0.0.1:8545 SAMPLE=500 BLOCK=latest CHECK_CHAIN_ADVANCED=1 \
        bash $REPO/scripts/verify-bloatnet.sh \
        > $logdir/verify-post.log 2>&1 || true
    local post_pass=$(sed 's/\x1b\[[0-9;]*m//g' $logdir/verify-post.log | grep -c "^PASS" 2>/dev/null || true)
    local post_fail=$(sed 's/\x1b\[[0-9;]*m//g' $logdir/verify-post.log | grep -c "^FAIL" 2>/dev/null || true)
    post_pass=${post_pass:-0}
    post_fail=${post_fail:-0}
    echo "    post-spamoor: $post_pass passed / $post_fail failed"

    # 7. Capture stats
    local genesis_root=$(cast block 0 --rpc-url http://127.0.0.1:8545 --field stateRoot 2>/dev/null || echo "unknown")
    local latest_root=$(cast block latest --rpc-url http://127.0.0.1:8545 --field stateRoot 2>/dev/null || echo "unknown")
    local latest_bn=$(cast block-number --rpc-url http://127.0.0.1:8545 2>/dev/null || echo "0")
    local db_apparent=$(du -sh --apparent-size $data | cut -f1)
    local db_actual=$(du -sh $data | cut -f1)

    cat > $results/$client-result.json <<JSON
{
  "client": "$client",
  "genesis_state_root": "$genesis_root",
  "post_spamoor_state_root": "$latest_root",
  "post_spamoor_block_number": $latest_bn,
  "pre_verify_pass": $pre_pass,
  "pre_verify_fail": $pre_fail,
  "post_verify_pass": $post_pass,
  "post_verify_fail": $post_fail,
  "db_size_apparent": "$db_apparent",
  "db_size_actual": "$db_actual"
}
JSON
    echo "    genesis_root: $genesis_root"
    echo "    latest_root:  $latest_root"
    echo "    latest_bn:    $latest_bn"
    echo "    db_size:      apparent=$db_apparent actual=$db_actual"

    # 8. Cleanup
    docker stop $ct >/dev/null 2>&1 || true
    docker rm $ct >/dev/null 2>&1 || true
    stop_engine_driver $logdir

    # 9. Free DB (free next client's disk budget). KEEP_DBS=1 preserves
    # them across the loop — useful for post-run inspection or follow-up
    # spamoor/verify cycles. Disk budget then becomes the caller's
    # problem; the bench host needs ~150-200 GB per client retained.
    if [ -z "${KEEP_DBS:-}" ]; then
        rm -rf $data
    else
        echo "    KEEP_DBS=$KEEP_DBS — preserving $data"
    fi
    echo "=== $client done ($(date)) ==="
}

# ── Main loop ──────────────────────────────────────────────────────────
overall_start=$(date +%s)
for client in $CLIENTS; do
    if ! run_one_client $client; then
        echo "${client} FAILED — continuing to next"
    fi
done
overall_end=$(date +%s)

echo
echo "═══════════════════════════════════════════════"
echo " Overall wall time: $((overall_end - overall_start)) seconds"
echo "═══════════════════════════════════════════════"

# ── Phase 4: cross-client state-root invariance gate ─────────────────
# THE primary success criterion: same YAML through every client must
# produce the same genesis state root. Cryptographically proves every
# (addr, balance, nonce, codeHash, storageRoot) tuple — and every byte
# of every storage slot — is identical across clients.
echo
echo "═══════════════════════════════════════════════════════════════"
echo " Cross-client genesis state-root invariance"
echo "═══════════════════════════════════════════════════════════════"
INV_GREEN=$'\033[0;32m'; INV_RED=$'\033[0;31m'; INV_YELLOW=$'\033[0;33m'; INV_RESET=$'\033[0m'
inv_ref=""
inv_fail=0
for c in $CLIENTS; do
    inv_file=$WORK/results/$c-result.json
    if [ ! -f "$inv_file" ]; then
        echo "  ${INV_YELLOW}SKIP${INV_RESET} $c (no result.json — boot/spamoor failed)"
        inv_fail=1
        continue
    fi
    inv_root=$(jq -r .genesis_state_root "$inv_file" 2>/dev/null)
    if [ -z "$inv_root" ] || [ "$inv_root" = "null" ]; then
        echo "  ${INV_YELLOW}SKIP${INV_RESET} $c (genesis_state_root missing or unparsable)"
        inv_fail=1
        continue
    fi
    if [ -z "$inv_ref" ]; then
        inv_ref=$inv_root
        echo "  ref:   $c  $inv_root"
    elif [ "$inv_root" = "$inv_ref" ]; then
        echo "  ${INV_GREEN}MATCH${INV_RESET} $c  $inv_root"
    else
        echo "  ${INV_RED}DIVERGE${INV_RESET} $c  $inv_root  (expected $inv_ref)"
        inv_fail=1
    fi
done
echo
if [ $inv_fail -eq 0 ]; then
    echo "${INV_GREEN}=== CROSS-CLIENT INVARIANCE: PASS ===${INV_RESET}"
else
    echo "${INV_RED}=== CROSS-CLIENT INVARIANCE: FAIL ===${INV_RESET}"
fi

echo
echo "=== DB size summary ==="
for c in $CLIENTS; do
    if [ -f $WORK/results/$c-result.json ]; then
        ap=$(jq -r .db_size_apparent $WORK/results/$c-result.json)
        ac=$(jq -r .db_size_actual $WORK/results/$c-result.json)
        echo "  $c: apparent=$ap actual=$ac"
    fi
done

echo
echo "Results: $WORK/results/"
ls -la $WORK/results/
