#!/bin/bash
# verify-cross-client-root.sh — asserts that every supplied client's
# genesis state root matches the canonical reference (the first URL is
# the reference). Use this for ad-hoc cross-client verification against
# LIVE RPCs, separate from the bench's master-loop result.json gate
# (which is in scripts/run-bloatnet.sh's Phase 4).
#
# Usage:
#   ./verify-cross-client-root.sh \
#     http://geth-host:8545 \
#     http://reth-host:8545 \
#     http://besu-host:8545 \
#     http://neth-host:8545
#
# Exits 0 on full match; 1 if any root diverges or a query fails.

set -euo pipefail

if [ $# -lt 2 ]; then
    echo "usage: $0 <ref-rpc-url> [other-rpc-url ...]" >&2
    exit 2
fi

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[0;33m'; RESET=$'\033[0m'

ref=""
fail=0
for rpc in "$@"; do
    root=$(cast block 0 --rpc-url "$rpc" --field stateRoot 2>/dev/null || true)
    if [ -z "$root" ]; then
        echo "  ${YELLOW}SKIP${RESET} $rpc (no response or eth_getBlockByNumber failed)"
        fail=1
        continue
    fi
    if [ -z "$ref" ]; then
        ref=$root
        echo "  ref:   $rpc  $root"
    elif [ "$root" = "$ref" ]; then
        echo "  ${GREEN}MATCH${RESET} $rpc  $root"
    else
        echo "  ${RED}DIVERGE${RESET} $rpc  $root  (expected $ref)"
        fail=1
    fi
done

echo
if [ $fail -eq 0 ]; then
    echo "${GREEN}=== CROSS-CLIENT INVARIANCE: PASS ===${RESET}"
    exit 0
fi
echo "${RED}=== CROSS-CLIENT INVARIANCE: FAIL ===${RESET}"
exit 1
