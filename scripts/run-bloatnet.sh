#!/bin/bash
# run-bloatnet.sh — orchestrator for the 100 GB bloatnet bench.
# Bench-host-only: requires ≥100 GB disk, ≥64 GB RAM, hours of wall time
# and companion tooling under $REPO/scripts/ (engine-driver/,
# gen-bloatnet-spec/, verify-bloatnet.sh) that is NOT checked into this
# repo. CI parity at ~100 MB scale lives in cross-client-genesis-root.
# Prereqs on PATH: docker, cast (foundry), spamoor, go.
# Required env: WORK, STATE_ACTOR_REPO, CLIENTS, SEED, ARCHIVE.

set -euo pipefail

export PATH=$HOME/.foundry/bin:/usr/local/go/bin:$PATH

WORK=${WORK:-$HOME/work/bloatnet}
REPO=${STATE_ACTOR_REPO:-$HOME/state-actor}
CLIENTS=${CLIENTS:-geth reth nethermind besu erigon ethrex}
# ethrex image must include --skip-genesis-validation (lambdaclass/ethrex#6783).
# Pin to a digest once a release ships it; :main is the post-merge interim tag.
ETHREX_IMAGE=${ETHREX_IMAGE:-ghcr.io/lambdaclass/ethrex:main}
SPEC_TARGET_GB=${SPEC_TARGET_GB:-25}
SPEC=$WORK/spec-bloatnet-${SPEC_TARGET_GB}gb.yaml
SEED=${SEED:-42}
SPAMOOR_PRIVKEY=0x0000000000000000000000000000000000000000000000000000000000000001
SPAMOOR_TARGET_BLOCK_DELTA=${SPAMOOR_BLOCKS:-500}
# ARCHIVE=1: state-actor + geth boot in archive mode (besu/nethermind ignore).
ARCHIVE=${ARCHIVE:-}

# Fail-fast for the bench-only companion tooling that lives outside this repo.
for path in \
    "$REPO/scripts/engine-driver/main.go" \
    "$REPO/scripts/verify-bloatnet.sh" \
    "$REPO/scripts/gen-bloatnet-spec/main.go"; do
    if [ ! -e "$path" ]; then
        echo "missing bench-only companion tooling: $path" >&2
        echo "This script is for the bench host only; CI parity runs in .github/workflows/ci.yml." >&2
        exit 1
    fi
done

mkdir -p $WORK/{logs,data,results}

for cmd in docker cast spamoor go; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "missing: $cmd" >&2; exit 1
    fi
done

if [ ! -s "$SPEC" ]; then
    echo "=== generating spec → $SPEC ==="
    cd $REPO
    go run ./scripts/gen-bloatnet-spec -out $SPEC -seed 4242 -target-gb $SPEC_TARGET_GB
fi
echo "=== spec: $(ls -lh $SPEC | awk '{print $5}') ==="

ENGINE_DRIVER=$WORK/bin/engine-driver
mkdir -p $WORK/bin
# Rebuild if missing, or if main.go or any internal/engineapi source
# is newer (engine-driver embeds internal/engineapi.EngineDriver, so
# changes there must trigger a rebuild — bit attempt 16).
need_rebuild=0
if [ ! -x "$ENGINE_DRIVER" ]; then
    need_rebuild=1
elif [ "$REPO/scripts/engine-driver/main.go" -nt "$ENGINE_DRIVER" ]; then
    need_rebuild=1
elif [ -n "$(find "$REPO/internal/engineapi" -type f -name '*.go' -newer "$ENGINE_DRIVER" -print -quit 2>/dev/null)" ]; then
    need_rebuild=1
fi
if [ "$need_rebuild" = 1 ]; then
    echo "=== building engine-driver ==="
    cd $REPO
    go build -o $ENGINE_DRIVER ./scripts/engine-driver/
fi

# Reth requires --authrpc.jwtsecret (raw 32-byte HMAC-SHA256 key, hex).
# besu/nethermind boot with --engine-jwt-disabled and ignore it.
JWT_HEX=$WORK/jwt.hex
if [ ! -s "$JWT_HEX" ]; then
    echo "=== generating engine-API JWT secret → $JWT_HEX ==="
    openssl rand -hex 32 > $JWT_HEX
    chmod 0644 $JWT_HEX
fi

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

# count_verify <log> <prefix> — strip ANSI, count lines beginning with prefix.
# grep -c always prints a number; `|| true` swallows the "no match" exit (1)
# without emitting an extra "0" that would corrupt the caller's arithmetic.
count_verify() {
    sed 's/\x1b\[[0-9;]*m//g' "$1" 2>/dev/null | grep -c "^$2" 2>/dev/null || true
}

# write_result <client> <status> <detail> <out_path>
# Reads metric vars (genesis_root, latest_root, latest_bn, pre_pass/fail,
# post_pass/fail, db_apparent, db_actual) from the caller's scope; missing
# vars default to "unknown" / 0 so partial-failure paths still emit valid JSON.
write_result() {
    local client=$1 status=$2 detail=$3 out=$4
    cat > "$out" <<JSON
{
  "client": "$client",
  "status": "$status",
  "status_detail": "$detail",
  "genesis_state_root": "${genesis_root:-unknown}",
  "post_spamoor_state_root": "${latest_root:-unknown}",
  "post_spamoor_block_number": ${latest_bn:-0},
  "pre_verify_pass": ${pre_pass:-0},
  "pre_verify_fail": ${pre_fail:-0},
  "post_verify_pass": ${post_pass:-0},
  "post_verify_fail": ${post_fail:-0},
  "db_size_apparent": "${db_apparent:-unknown}",
  "db_size_actual": "${db_actual:-unknown}"
}
JSON
}

boot_client() {
    local client=$1 data=$2 ct=$3
    echo "=== booting $client (container=$ct, data=$data) ==="
    case $client in
        geth)
            local gc_args=""
            [ -n "$ARCHIVE" ] && gc_args="--gcmode=archive"
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
            # --genesis-state-hash-cache-enabled: trust DB stateRoot; alloc={}
            # so a recompute would diverge (see client/besu/chainspec.go).
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                hyperledger/besu:26.5.0 \
                --genesis-file=/data/besu-chainspec.json \
                --genesis-state-hash-cache-enabled \
                --data-storage-format=BONSAI \
                --data-path=/data \
                --rpc-http-enabled --rpc-http-host=127.0.0.1 --rpc-http-port=8545 \
                --rpc-http-api=ETH,NET,WEB3,TXPOOL,DEBUG \
                --host-allowlist=* \
                --engine-rpc-enabled --engine-host-allowlist=* \
                --engine-rpc-port=8551 \
                --engine-jwt-disabled
            ;;
        nethermind)
            # Boot via boot.cfg per docs/RUNBOOK.md — Init.BaseDbPath must
            # equal the datadir or Nethermind creates an empty DB at default.
            cat > $data/boot.cfg << 'NETH_CFG'
{
  "Init": {
    "EnableUnsecuredDevWallet": true,
    "KeepDevWalletInMemory": true,
    "DiscoveryEnabled": false,
    "PeerManagerEnabled": false,
    "ChainSpecPath": "/data/parity-chainspec.json",
    "BaseDbPath": "/data",
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
    "Host": "127.0.0.1",
    "Port": 8545,
    "EnabledModules": ["Eth", "Net", "Web3"],
    "EngineHost": "127.0.0.1",
    "EnginePort": 8551,
    "EngineEnabledModules": ["Engine", "Eth", "Net", "Web3", "Subscribe"],
    "UnsecureDevNoRpcAuthentication": true
  },
  "Metrics": { "Enabled": false },
  "Merge": { "Enabled": true, "TerminalTotalDifficulty": "0" },
  "Mining": { "Enabled": false }
}
NETH_CFG
            # Flat mode must be opted into at boot; a flat datadir booted
            # without this flag detects as patricia and finds an empty state DB.
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                nethermind/nethermind:1.39.0 \
                --config=/data/boot.cfg \
                --FlatDb.Enabled=true \
                --log=Info
            ;;
        reth)
            # --dev.block-time=1s drives the local miner on a wall clock;
            # without it --dev uses MiningMode::Instant and deadlocks
            # against spamoor's funding phase.
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                ghcr.io/paradigmxyz/reth@sha256:e528857e5e9ebc2c6cb99f28436e70ded38ca905629f00afc98d186e27d206e0 \
                node --dev --dev.block-time=1s --debug.skip-genesis-validation \
                --datadir /data \
                --http --http.addr=127.0.0.1 --http.port=8545
            ;;
        erigon)
            # Erigon v3.4.2 dev-mode flag set:
            #   --dev.period 2    wall-clock block production (2 s slot)
            #   --networkid 1337  match the chainID in genesis.json
            #   --no-downloader   skip snapshot peer download
            #   --nodiscover      disable peer discovery
            #   --port :0         random p2p port (no inbound peers
            #                     needed for a dev-mode bench)
            #   --http etc.       RPC config matching besu/reth defaults
            #
            # We deliberately DO NOT pass --chain dev:
            # v3.4.2's daemon path rejects "--chain dev" against an
            # existing-chaindata boot with "Fatal: chain name is not
            # recognized: dev" (a code-path quirk where the dev short-
            # circuit at flags.go:1923 does not apply during the
            # populated-DB validation). Letting --chain default to
            # "mainnet" + --networkid 1337 makes the daemon honor the
            # chain config from MDBX (chainID 1337, post-Prague forks)
            # rather than trying to migrate to a built-in dev config.
            #
            # The main-branch flags (--dev-validator-seed /
            # --dev-validator-count / --dev.slot-time) DO NOT EXIST in
            # v3.4.2 — Erigon's main rewrote dev-mode bootstrapping
            # post-tag.
            # v3.4.2's --chain dev + --dev.period mode failed to mine
            # blocks against state-actor's init'd chaindata (block 0
            # forever). The mechanism likely expects a fresh dev-chain
            # genesis, not our custom-init'd one. Bench iteration
            # confirmed this empirically across 5 boot variants.
            #
            # Solution: drive block production via Engine API (same as
            # besu / nethermind). engine-driver issues
            # engine_forkchoiceUpdated + engine_newPayload calls on a
            # wall-clock interval. erigon executes them via its
            # consensus-less PoS-style stage runner.
            #
            # Flags:
            #   --networkid 1337         match chainID in genesis.json
            #   --authrpc.addr 127.0.0.1 engine API listener
            #   --authrpc.port 8551
            #   --no-downloader          skip snapshot peer download
            #   --port 0                 random p2p port
            #   --nodiscover             disable peer discovery
            #   --http.*                 eth RPC for spamoor + verify
            #
            # We deliberately omit --chain dev (v3.4.2 daemon fails
            # with "chain name is not recognized: dev" against
            # populated chaindata; see flags.go:1923) and let chain
            # config come from MDBX.
            #
            # JWT file pre-existing at /data/jwt.hex (created by the
            # bench script's setup phase). The erigon container needs
            # --authrpc.jwtsecret to enable Engine API; geth/reth use
            # different mechanisms.
            cp $JWT_HEX $data/jwt.hex 2>/dev/null || true
            chmod 0644 $data/jwt.hex 2>/dev/null || true
            # --externalcl: required because v3.4.2's --chain dev path
            # is broken (PoW mining was removed in #17813; no block
            # production without external CL). Per Erigon issue #18827
            # the workaround is to drive block production via Engine
            # API + an external consensus-layer mock — exactly what
            # our engine-driver provides.
            # --snap.stop / --snap.state.stop: critical for state-actor
            # boot. Without them, erigon's `StageSnapshots` stage tries
            # to wait for snapshot downloads from peers (even with
            # --no-downloader) and `engine_forkchoiceUpdated` calls
            # respond SYNCING forever instead of accepting our genesis
            # as head. Discovered via bench attempt 14:
            #   "[rpc] download of segments not complete yet. please
            #   wait StageSnapshots to finish"
            # Daemon uses the state-actor-erigon image's locally-built
            # erigon binary (stock upstream, pinned commit 14273f79a6 — no
            # patches). Override the image's default entrypoint to
            # /usr/local/bin/erigon since state-actor-erigon is a
            # multi-purpose image (state-actor binary + erigon binary).
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                --entrypoint /usr/local/bin/erigon \
                state-actor-erigon:latest \
                --datadir /data \
                --networkid 1337 \
                --no-downloader \
                --snap.stop \
                --snap.state.stop \
                --externalcl \
                --authrpc.addr 127.0.0.1 \
                --authrpc.port 8551 \
                --authrpc.jwtsecret /data/jwt.hex \
                --port 0 \
                --http --http.addr=127.0.0.1 --http.port=8545 \
                --http.api=eth,net,web3,txpool,debug \
                --nodiscover
            ;;
        ethrex)
            # ethrex has no self-mining dev mode usable here, so it is
            # engine-driven like besu/nethermind — but it REQUIRES JWT on
            # authrpc (it cannot disable it), so the engine-driver must sign
            # (see start_engine_driver_if_needed). --skip-genesis-validation
            # makes ethrex trust the state-actor-written stateRoot instead of
            # recomputing from the empty-alloc sidecar (lambdaclass/ethrex#6783);
            # $ETHREX_IMAGE must include that flag. --syncmode full is required:
            # in the default snap mode ethrex returns SYNCING + null payloadId for
            # every engine forkchoiceUpdated, so the driver can never build.
            # $JWT_HEX is generated once at script start (openssl rand, see top
            # of file); RUNBOOK.md documents the engine-API JWT requirement.
            cp "$JWT_HEX" "$data/jwt.hex"
            docker run -d --name $ct \
                --network host \
                -v $data:/data \
                $ETHREX_IMAGE \
                --network /data/ethrex-genesis.json \
                --datadir /data \
                --skip-genesis-validation \
                --syncmode full \
                --http.addr 127.0.0.1 --http.port 8545 \
                --http.api eth,net,web3 \
                --authrpc.addr 127.0.0.1 --authrpc.port 8551 \
                --authrpc.jwtsecret /data/jwt.hex
            ;;
        *)
            echo "unknown client: $client" >&2; return 1 ;;
    esac
}

# Starts engine-driver for besu+nethermind+erigon (geth+reth self-mine
# via --dev.*). Returns 1 if the driver dies within 1s of launch (bad
# config, port wedged). Erigon v3.4.2 needs the engine-driver because
# its --chain dev + --dev.period block production fails to advance
# blocks against state-actor's custom-init'd chaindata (see bench
# attempt-12 finding); the Engine API path works around this.
start_engine_driver_if_needed() {
    local client=$1 logdir=$2
    case $client in
        besu|nethermind|erigon|ethrex) ;;
        *) return 0 ;;
    esac
    echo "=== starting engine-driver for $client ==="
    # erigon and ethrex both enforce JWT on authrpc (besu/nethermind run with
    # it disabled and ignore the -jwt arg). The driver signs engine calls with
    # the same secret the container reads at /data/jwt.hex.
    local jwt_arg=""
    case $client in erigon|ethrex) jwt_arg="-jwt $JWT_HEX" ;; esac
    nohup $ENGINE_DRIVER \
        -engine http://127.0.0.1:8551 \
        -eth http://127.0.0.1:8545 \
        -fork osaka \
        -block-time 1s \
        $jwt_arg \
        > $logdir/engine-driver.log 2>&1 &
    local pid=$!
    echo $pid > $logdir/engine-driver.pid
    sleep 1
    if ! kill -0 $pid 2>/dev/null; then
        echo "engine-driver exited immediately (pid=$pid); see $logdir/engine-driver.log" >&2
        tail -5 $logdir/engine-driver.log >&2 || true
        rm -f $logdir/engine-driver.pid
        return 1
    fi
}

stop_engine_driver() {
    local logdir=$1
    if [ -f $logdir/engine-driver.pid ]; then
        kill $(cat $logdir/engine-driver.pid) 2>/dev/null || true
        rm -f $logdir/engine-driver.pid
    fi
}

run_one_client() {
    local client=$1
    local data=$WORK/data/$client
    local logdir=$WORK/logs/$client
    local results=$WORK/results
    local ct="bloatnet-$client"
    local result_json=$results/$client-result.json

    mkdir -p $data $logdir $results

    # Result fields, populated as the run progresses. Declared up-front so
    # every write_result call (including early-return paths) sees them.
    local genesis_root="unknown"
    local latest_root="unknown"
    local latest_bn=0
    local pre_pass=0 pre_fail=0
    local post_pass=0 post_fail=0
    local db_apparent="unknown" db_actual="unknown"

    echo
    echo "════════════════════════════════════════════════════════════════"
    echo " $client  ($(date))"
    echo "════════════════════════════════════════════════════════════════"

    # geth uses <datadir>/geth/chaindata; others use the bare datadir.
    local db_path="/data"
    [ "$client" = "geth" ] && db_path="/data/geth/chaindata"

    # --archive is geth/reth/erigon only (besu/nethermind reject at parse;
    # erigon accepts it as a no-op per genesis/forks.go MaxForkForClient
    # and main.go's allow-list — the snapshot tier is archive-by-design
    # once history files ship, and absent that the value-domain reads
    # degrade gracefully).
    local archive_arg=""
    if [ -n "$ARCHIVE" ]; then
        case $client in geth|reth|erigon) archive_arg="--archive" ;; esac
    fi

    echo "=== state-actor generation (writing to $data, container path $db_path${archive_arg:+, archive=on}) ==="
    local gen_status=0
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
        --target-size=25GB \
        $archive_arg \
        --verbose \
        > $logdir/gen.log 2>&1 || gen_status=$?
    if [ $gen_status -ne 0 ]; then
        echo "state-actor generation FAILED (exit $gen_status). See $logdir/gen.log" >&2
        tail -20 $logdir/gen.log >&2 || true
        write_result "$client" "gen_failed" "exit=$gen_status; see $logdir/gen.log" "$result_json"
        return 1
    fi
    echo "=== generation done; DB size: $(du -sh $data | cut -f1) ==="

    docker rm -f $ct 2>/dev/null || true
    boot_client $client $data $ct

    # wait_for_rpc MUST precede start_engine_driver_if_needed — the driver
    # polls 127.0.0.1:8545 immediately and exits on first connection-refused.
    local rpc_timeout=300
    case $client in besu|nethermind) rpc_timeout=900 ;; esac
    if ! wait_for_rpc "http://127.0.0.1:8545" $rpc_timeout; then
        docker logs --tail 50 $ct >&2 || true
        docker rm -f $ct 2>/dev/null || true
        write_result "$client" "rpc_timeout" "after ${rpc_timeout}s; see container logs" "$result_json"
        return 1
    fi

    # Flat-DB boot proof: Nethermind >= 1.39.0 logs its selected state backend
    # at startup. A flat run MUST detect flat; a silent patricia fallback would
    # leave the state unreadable, so fail the leg loudly rather than benchmark
    # an empty node.
    if [ "$client" = "nethermind" ]; then
        if docker logs $ct 2>&1 | grep -qiE 'State backend: flat'; then
            echo "    nethermind: flat backend detected"
        else
            echo "    nethermind: FLAT BACKEND NOT DETECTED — see docker logs $ct" >&2
            docker logs --tail 50 $ct >&2 || true
            docker rm -f $ct 2>/dev/null || true
            write_result "$client" "flat_not_detected" "boot did not log 'State backend: flat'" "$result_json"
            return 1
        fi
    fi

    if ! start_engine_driver_if_needed $client $logdir; then
        docker rm -f $ct 2>/dev/null || true
        write_result "$client" "engine_driver_died" "see $logdir/engine-driver.log" "$result_json"
        return 1
    fi

    echo "=== pre-spamoor verify ==="
    local pre_verify_status=0
    RPC=http://127.0.0.1:8545 SAMPLE=500 BLOCK=latest \
        bash $REPO/scripts/verify-bloatnet.sh \
        > $logdir/verify-pre.log 2>&1 || pre_verify_status=$?
    pre_pass=$(count_verify $logdir/verify-pre.log PASS)
    pre_fail=$(count_verify $logdir/verify-pre.log FAIL)
    echo "    pre-spamoor:  $pre_pass passed / $pre_fail failed (exit=$pre_verify_status)"

    # spamoor erc20_bloater. Background, poll eth_blockNumber, SIGTERM
    # when tip advances by SPAMOOR_TARGET_BLOCK_DELTA. Mid-bench RPC death
    # is detected via consecutive cast failures (5 strikes → abort).
    local start_tip
    start_tip=$(cast block-number --rpc-url http://127.0.0.1:8545 2>/dev/null || echo "")
    if ! [[ "$start_tip" =~ ^[0-9]+$ ]]; then
        genesis_root=$(cast block 0 --rpc-url http://127.0.0.1:8545 --field stateRoot 2>/dev/null || echo "unknown")
        write_result "$client" "rpc_died_before_spamoor" "start_tip='$start_tip'" "$result_json"
        docker rm -f $ct 2>/dev/null || true
        stop_engine_driver $logdir
        return 1
    fi
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

    local poll_deadline=$(( $(date +%s) + 1800 ))
    local cur="$start_tip"
    local rpc_fails=0
    local stage_status="ok"
    while [ $(date +%s) -lt $poll_deadline ]; do
        local probe
        probe=$(cast block-number --rpc-url http://127.0.0.1:8545 2>/dev/null || echo "")
        if [[ "$probe" =~ ^[0-9]+$ ]]; then
            cur="$probe"
            rpc_fails=0
            if [ $cur -ge $target_tip ]; then
                echo "    reached target tip ($cur >= $target_tip)"
                break
            fi
        else
            rpc_fails=$((rpc_fails + 1))
            if [ $rpc_fails -ge 5 ]; then
                echo "    RPC died mid-flood (5 consecutive cast failures)" >&2
                stage_status="rpc_died_midflood"
                break
            fi
        fi
        if ! kill -0 $spamoor_pid 2>/dev/null; then
            echo "    spamoor exited unexpectedly at tip=$cur"
            stage_status="spamoor_crashed"
            break
        fi
        sleep 5
    done
    if [ "$stage_status" = "ok" ] && [ $(date +%s) -ge $poll_deadline ] && [ $cur -lt $target_tip ]; then
        stage_status="spamoor_timeout"
    fi

    # Stop spamoor (SIGTERM, fallback SIGKILL after 10s) and capture exit.
    kill -TERM $spamoor_pid 2>/dev/null || true
    for i in $(seq 1 10); do
        if ! kill -0 $spamoor_pid 2>/dev/null; then break; fi
        sleep 1
    done
    kill -KILL $spamoor_pid 2>/dev/null || true
    local spamoor_exit=0
    wait $spamoor_pid 2>/dev/null || spamoor_exit=$?

    # Post-spamoor verify (only if RPC is still alive).
    local post_verify_status=0
    if [ "$stage_status" != "rpc_died_midflood" ]; then
        echo "=== post-spamoor verify ==="
        RPC=http://127.0.0.1:8545 SAMPLE=500 BLOCK=latest CHECK_CHAIN_ADVANCED=1 \
            bash $REPO/scripts/verify-bloatnet.sh \
            > $logdir/verify-post.log 2>&1 || post_verify_status=$?
        post_pass=$(count_verify $logdir/verify-post.log PASS)
        post_fail=$(count_verify $logdir/verify-post.log FAIL)
    fi
    echo "    post-spamoor: $post_pass passed / $post_fail failed (exit=$post_verify_status)"

    genesis_root=$(cast block 0 --rpc-url http://127.0.0.1:8545 --field stateRoot 2>/dev/null || echo "unknown")
    latest_root=$(cast block latest --rpc-url http://127.0.0.1:8545 --field stateRoot 2>/dev/null || echo "unknown")
    latest_bn=$(cast block-number --rpc-url http://127.0.0.1:8545 2>/dev/null || echo "0")
    db_apparent=$(du -sh --apparent-size $data | cut -f1)
    db_actual=$(du -sh $data | cut -f1)

    local detail=""
    [ "$stage_status" != "ok" ] && detail="spamoor_exit=$spamoor_exit; tip=$cur"
    write_result "$client" "$stage_status" "$detail" "$result_json"

    echo "    genesis_root: $genesis_root"
    echo "    latest_root:  $latest_root"
    echo "    latest_bn:    $latest_bn"
    echo "    db_size:      apparent=$db_apparent actual=$db_actual"
    echo "    status:       $stage_status"

    docker rm -f $ct 2>/dev/null || true
    stop_engine_driver $logdir

    # KEEP_DBS=1 preserves $data; otherwise only delete on clean success.
    if [ "$stage_status" = "ok" ] && [ -z "${KEEP_DBS:-}" ]; then
        rm -rf $data
    elif [ "$stage_status" != "ok" ]; then
        echo "    preserving $data (status=$stage_status)"
    fi
    echo "=== $client done ($(date)) ==="
}

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

# Cross-client genesis state-root invariance: same YAML through every
# client must produce the same root. Erigon uses HexPatriciaHashed and
# the rest use a standard MPT, but they are SPEC-equivalent on genesis
# input (proven byte-for-byte by
# internal/erigon/_fixtures/commitment/h4_test.go's
# TestH4_HexPatriciaHashed_MatchesMPT — both produce identical
# 32-byte roots over the same alloc). Compares only clients with
# status=ok.
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
        echo "  ${INV_YELLOW}SKIP${INV_RESET} $c (no result.json)"
        inv_fail=1
        continue
    fi
    inv_status=$(jq -r .status "$inv_file" 2>/dev/null || echo "unknown")
    inv_root=$(jq -r .genesis_state_root "$inv_file" 2>/dev/null)
    if [ "$inv_status" != "ok" ]; then
        echo "  ${INV_YELLOW}SKIP${INV_RESET} $c (status=$inv_status)"
        inv_fail=1
        continue
    fi
    if [ -z "$inv_root" ] || [ "$inv_root" = "null" ] || [ "$inv_root" = "unknown" ]; then
        echo "  ${INV_YELLOW}SKIP${INV_RESET} $c (genesis_state_root unparsable)"
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
        st=$(jq -r .status $WORK/results/$c-result.json)
        echo "  $c: apparent=$ap actual=$ac status=$st"
    fi
done

echo
echo "Results: $WORK/results/"
ls -la $WORK/results/
