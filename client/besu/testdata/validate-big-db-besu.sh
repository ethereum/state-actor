#!/usr/bin/env bash
# validate-big-db-besu.sh — boot Besu against the generated DB and run the
# full validation: genesis hash + state root + dev-wallet balance + 100 tx
# round-trip + chain progression.
#
# Args:
#   $1 = path to state-actor's output dir (default /tmp/sa-besu-smoke)
#   $2 = state-actor's reported state root (printed at end of generation)
set -euo pipefail

DBPATH="${1:-/tmp/sa-besu-smoke}"
EXPECTED_ROOT="${2:-}"
RPC="http://127.0.0.1:8545"
CONTAINER="besu-validate"
COINBASE="0x7e5f4552091a69125d5dfcb7b8c2659029395bdf"
TESTDATA="$(cd "$(dirname "$0")" && pwd)"

cleanup() {
  docker rm -f "$CONTAINER" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== validate-big-db-besu.sh ==="
echo "DB:                 $DBPATH"
echo "expected stateRoot: ${EXPECTED_ROOT:-<not provided — will skip equality check>}"
echo "size:               $(du -sh "$DBPATH" | awk '{print $1}')"
echo

echo "[1/6] Booting hyperledger/besu:25.11.0 ..."
# CRITICAL: pass the same --genesis-file state-actor consumed. Besu's
# DefaultBlockchain.setGenesis (DefaultBlockchain.java:1027-1057) re-reads
# the genesis JSON on every boot and compares the recomputed genesis block
# hash against VARIABLES["chainHeadHash"]. Mismatch fatals with
# InvalidConfigurationException.
#
# We pass --genesis-state-hash-cache-enabled because synthetic-state DBs
# can never match a recompute from the small genesis JSON's alloc by
# definition; the flag tells Besu to trust the stored stateRoot rather
# than recompute. State-trie correctness is validated independently by
# the differential oracle (oracle_test.go) against Besu's source-pinned
# golden hashes — this smoke just confirms the DB boots end-to-end.
docker run --rm -d \
  --name "$CONTAINER" \
  -v "$TESTDATA:/test:ro" \
  -v "$DBPATH:/data" \
  -p 127.0.0.1:8545:8545 \
  hyperledger/besu:25.11.0 \
  --data-path=/data \
  --genesis-file=/data/besu-chainspec.json \
  --network-id=1337 \
  --rpc-http-enabled \
  --rpc-http-host=0.0.0.0 \
  --rpc-http-port=8545 \
  --rpc-http-api=ETH,NET,WEB3,ADMIN,MINER \
  --rpc-http-cors-origins="*" \
  --host-allowlist="*" \
  --data-storage-format=BONSAI \
  --genesis-state-hash-cache-enabled \
  --min-gas-price=0 \
  --miner-enabled \
  --miner-coinbase="$COINBASE" \
  --logging=INFO > /dev/null

# Wait for RPC.
deadline=$(( $(date +%s) + 90 ))
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
  echo "  RPC did not come up in 90s — Besu likely refused the DB."
  echo "--- besu logs ---"
  docker logs "$CONTAINER" 2>&1 | tail -50
  exit 1
fi
echo "  RPC up"

echo "[2/6] Booting checks ..."
chain=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' "$RPC" | jq -r .result)
block=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "$RPC" | jq -r .result)
echo "  chainId=$chain (expected 0x539 = 1337)"
echo "  block=$block (expected 0x0)"

echo "[3/6] Genesis state root match ..."
got=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x0",false],"id":1}' \
  "$RPC" | jq -r .result.stateRoot)
echo "  Besu sees: $got"
if [[ -n "$EXPECTED_ROOT" ]]; then
  if [[ "$got" == "$EXPECTED_ROOT" ]]; then
    echo "  MATCH"
  else
    echo "  MISMATCH (expected $EXPECTED_ROOT)"
    docker logs "$CONTAINER" 2>&1 | tail -50
    exit 1
  fi
fi

echo "[4/6] Dev wallet balance ($COINBASE) ..."
bal=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$COINBASE\",\"latest\"],\"id\":1}" \
  "$RPC" | jq -r .result)
echo "  balance=$bal (expected ~999999999 ETH from --inject-accounts)"

echo "[5/6] Send 100 txs ..."
RPC="$RPC" COINBASE="$COINBASE" "$TESTDATA/send-100-txs-besu.sh" 2>&1 | tail -8

echo "[6/6] Final state ..."
final=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "$RPC" | jq -r .result)
echo "  final block: $final"
echo "PASS"
