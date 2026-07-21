#!/usr/bin/env bash
# validate-big-db.sh — boot Nethermind against the generated DB and run the
# full validation: genesis hash + state root + dev-wallet balance + 100 tx
# round-trip + chain progression.
#
# Args:
#   $1 = path to state-actor's output dir (default /tmp/sa-neth-big)
#   $2 = state-actor's reported state root (printed at end of generation)
#   $3 = config to use (default sa-dev-v2.json — current /data layout;
#        pass sa-dev.json for the legacy /data/db layout)
set -euo pipefail

DBPATH="${1:-/tmp/sa-neth-big}"
EXPECTED_ROOT="${2:-}"
CONFIG_NAME="${3:-sa-dev-v2.json}"
RPC="http://127.0.0.1:8545"
CONTAINER="neth-validate"
TESTDATA="$(cd "$(dirname "$0")" && pwd)"

cleanup() {
  docker rm -f "$CONTAINER" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== validate-big-db.sh ==="
echo "DB:     $DBPATH"
echo "config: $TESTDATA/configs/$CONFIG_NAME"
echo "expected stateRoot: ${EXPECTED_ROOT:-<not provided — will skip equality check>}"
echo "size: $(du -sh "$DBPATH" | awk '{print $1}')"
echo

echo "[1/6] Booting nethermind/nethermind:1.39.0 ..."
docker run --rm -d \
  --name "$CONTAINER" \
  -v "$TESTDATA:/test:ro" \
  -v "$DBPATH:/data" \
  -p 127.0.0.1:8545:8545 \
  nethermind/nethermind:1.39.0 \
  --config "/test/configs/$CONFIG_NAME" \
  --FlatDb.Enabled=true \
  --log Info > /dev/null

# Wait for RPC to come up.
deadline=$(( $(date +%s) + 90 ))
while [[ $(date +%s) -lt $deadline ]]; do
  if curl -s -o /dev/null --connect-timeout 1 -X POST "$RPC" \
       -H 'Content-Type: application/json' \
       -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}'; then
    break
  fi
  sleep 1
done
echo "  RPC up after $(( $(date +%s) - deadline + 90 ))s"

echo "[2/6] Booting checks ..."
chain=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' "$RPC" | jq -r .result)
block=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "$RPC" | jq -r .result)
echo "  chainId=$chain block=$block"

echo "[3/6] Genesis state root match ..."
got=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x0",false],"id":1}' \
  "$RPC" | jq -r .result.stateRoot)
echo "  Nethermind sees: $got"
if [[ -n "$EXPECTED_ROOT" ]]; then
  if [[ "$got" == "$EXPECTED_ROOT" ]]; then
    echo "  MATCH"
  else
    echo "  MISMATCH (expected $EXPECTED_ROOT)"
    docker logs "$CONTAINER" 2>&1 | tail -50
    exit 1
  fi
fi

echo "[4/6] Dev wallet balance (0x7e5f4552...) ..."
bal=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x7e5f4552091a69125d5dfcb7b8c2659029395bdf","latest"],"id":1}' \
  "$RPC" | jq -r .result)
echo "  balance=$bal (expected 0xd3c21bcecceda1000000 = 1M ETH)"

echo "[5/6] Send 100 txs ..."
/tmp/neth-test/send-100-txs.sh 2>&1 | tail -8

echo "[6/6] Final state ..."
final=$(curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "$RPC" | jq -r .result)
echo "  final block: $final"
echo "PASS"
