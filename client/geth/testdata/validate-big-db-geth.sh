#!/usr/bin/env bash
# validate-big-db-geth.sh — boot upstream geth against the generated DB
# and run the boot-readability smoke: chainId + state root + dev-wallet
# balance reads via JSON-RPC.
#
# Mirrors validate-big-db-besu.sh but skips the tx-sending step:
# post-merge geth does not auto-mine without a consensus layer, and the
# state-actor MPT path's invariant ("geth boots and reads the DB
# correctly") doesn't depend on mining. Cross-client root invariance
# (canonical-MPT) is covered by client/geth's TestPopulateCanonicalEntitygenRoot
# in-process; this smoke confirms the on-disk layout is what stock geth
# expects.
#
# Args:
#   $1 = path to state-actor's output dir (default /tmp/sa-geth-smoke)
#   $2 = state-actor's reported state root (printed at end of generation)
set -euo pipefail

DBPATH="${1:-/tmp/sa-geth-smoke}"
EXPECTED_ROOT="${2:-}"
RPC="http://127.0.0.1:8545"
CONTAINER="geth-validate"
GETH_IMAGE="${GETH_IMAGE:-ethereum/client-go:v1.17.2}"
COINBASE="0x7e5f4552091a69125d5dfcb7b8c2659029395bdf"
DEV2="0x2b5ad5c4795c026514f8317c7a215e218dccd6cf"
TESTDATA="$(cd "$(dirname "$0")" && pwd)"

cleanup() {
  docker rm -f "$CONTAINER" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== validate-big-db-geth.sh ==="
echo "DB:                 $DBPATH"
echo "image:              $GETH_IMAGE"
echo "expected stateRoot: ${EXPECTED_ROOT:-<not provided — will skip equality check>}"
echo "size:               $(du -sh "$DBPATH" | awk '{print $1}')"
echo

echo "[1/5] Booting $GETH_IMAGE ..."
# State-actor wrote the genesis block + chain config via WriteGenesisBlock,
# so we DO NOT run `geth init`. The boot path reads the chain config from
# rawdb (via the persisted block.Hash() → ChainConfig mapping) and uses
# the SnapshotRoot / SnapshotGenerator metadata to skip regeneration.
#
# --syncmode=full keeps the node passive: no peer discovery, no PoS CL
# attached, just serve RPC reads against the DB. --networkid matches
# the chainId baked into the alloc'd genesis (1337).
docker run --rm -d \
  --name "$CONTAINER" \
  -v "$DBPATH:/data" \
  -p 127.0.0.1:8545:8545 \
  "$GETH_IMAGE" \
  --datadir=/data \
  --db.engine=pebble \
  --networkid=1337 \
  --syncmode=full \
  --nodiscover \
  --maxpeers=0 \
  --http --http.addr=0.0.0.0 --http.port=8545 --http.api=eth,net,web3,debug \
  --http.corsdomain="*" --http.vhosts="*" \
  --verbosity=3 > /dev/null

# Wait for RPC.
deadline=$(( $(date +%s) + 60 ))
ready=0
while [[ $(date +%s) -lt $deadline ]]; do
  if curl -s -o /dev/null --connect-timeout 1 -X POST "$RPC" \
       -H 'Content-Type: application/json' \
       -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}'; then
    ready=1
    break
  fi
  sleep 1
done
if [[ $ready -eq 0 ]]; then
  echo "  RPC did not come up in 60s — geth likely refused the DB."
  echo "--- geth logs ---"
  docker logs "$CONTAINER" 2>&1 | tail -80
  exit 1
fi
echo "  RPC up"

echo "[2/5] Booting checks ..."
chain=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' "$RPC" | jq -r .result)
block=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "$RPC" | jq -r .result)
echo "  chainId=$chain (expected 0x539 = 1337)"
echo "  block=$block (expected 0x0)"
if [[ "$chain" != "0x539" ]]; then
  echo "  chainId mismatch — DB's persisted ChainConfig is wrong"
  docker logs "$CONTAINER" 2>&1 | tail -50
  exit 1
fi
if [[ "$block" != "0x0" ]]; then
  echo "  block != 0 — geth re-synced or rolled forward unexpectedly"
  exit 1
fi

echo "[3/5] Genesis state root match ..."
got=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x0",false],"id":1}' \
  "$RPC" | jq -r .result.stateRoot)
echo "  geth sees: $got"
if [[ -n "$EXPECTED_ROOT" ]]; then
  if [[ "$got" == "$EXPECTED_ROOT" ]]; then
    echo "  MATCH"
  else
    echo "  MISMATCH (expected $EXPECTED_ROOT)"
    docker logs "$CONTAINER" 2>&1 | tail -50
    exit 1
  fi
fi

echo "[4/5] Dev wallet balances ..."
# After the --genesis JSON flow was retired, dev wallets are pre-funded
# via --inject-accounts (state-actor's hardcoded 999999999 ETH). The
# strict balance equality is gone since the value is now an
# implementation detail of the inject path; we just assert the address
# resolves to a non-zero balance, which is the actual smoke property
# (the inject-account write made it into the snapshot).
bal_coinbase=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$COINBASE\",\"latest\"],\"id\":1}" \
  "$RPC" | jq -r .result)
echo "  $COINBASE: $bal_coinbase (expected non-zero from --inject-accounts)"
if [[ "$bal_coinbase" == "0x0" || -z "$bal_coinbase" || "$bal_coinbase" == "null" ]]; then
  echo "  coinbase balance is zero — --inject-accounts did not land"
  exit 1
fi

bal_dev2=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$DEV2\",\"latest\"],\"id\":1}" \
  "$RPC" | jq -r .result)
echo "  $DEV2: $bal_dev2 (expected non-zero from --inject-accounts)"
if [[ "$bal_dev2" == "0x0" || -z "$bal_dev2" || "$bal_dev2" == "null" ]]; then
  echo "  dev2 balance is zero — --inject-accounts did not land"
  exit 1
fi

echo "[5/5] Sample synthetic-state read ..."
# Read eth_getBalance for ANY address that's not in the genesis alloc.
# A successful (non-zero) result confirms the snapshot layer is wired
# correctly to the trie and the random-state-only addresses survived
# Phase 2's keccak-sorted writes. We use a known synthetic address
# fingerprint by parsing state-actor's stdout (the smoke target writes
# this to $DBPATH/smoke.log so we can grep here).
sample_log="$DBPATH/smoke.log"
if [[ -f "$sample_log" ]]; then
  sample_eoa=$(grep -E '^\s+EOA #1:\s+0x' "$sample_log" | awk '{print $NF}' | head -1)
  if [[ -n "$sample_eoa" ]]; then
    sample_bal=$(curl -s -X POST -H 'Content-Type: application/json' \
      --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$sample_eoa\",\"latest\"],\"id\":1}" \
      "$RPC" | jq -r .result)
    echo "  synthetic EOA $sample_eoa: $sample_bal"
    if [[ "$sample_bal" == "0x0" || -z "$sample_bal" || "$sample_bal" == "null" ]]; then
      echo "  synthetic EOA has zero balance — snapshot layer may not be reachable"
      exit 1
    fi
  else
    echo "  no synthetic-EOA sample in $sample_log; skipping"
  fi
else
  echo "  $sample_log not found; skipping (run via make smoke-geth to capture)"
fi

echo "PASS: geth boots and reads state-actor's geth-MPT DB end-to-end"
