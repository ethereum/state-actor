#!/usr/bin/env bash
# verify-spec.sh — fail-fast RPC verifier for state-actor's --spec output.
#
# Shared between CI and the bench host. Walks the YAML at $SPEC, derives
# each entity's address (literal / name-derived / position-derived,
# matching internal/specbuild/derive.go), then asserts balance / nonce /
# code / template-output via cast. Plus fixed assertions: chain-id,
# canonical system contracts, and (in MODE=post) BEACON_ROOTS ring
# buffer + chain advance + spamoor sender nonce > 0.
#
# First failing assertion aborts the script with a non-zero exit.
# CI's workflow step inherits the exit code; the bench-host orchestrator
# checks $? on the call.
#
# Env-vars (every var has a default that lets `bash verify-spec.sh`
# succeed against a fresh dev node):
#   RPC     JSON-RPC endpoint        (default http://127.0.0.1:8545)
#   SPEC    Path to spec YAML        (default examples/spec-ci-comprehensive.yaml)
#   SEED    Seed for address deriv.  (default 0, matching internal/e2e_testing.CISpecSeed)
#   MODE    pre | post               (default pre)
#   BLOCK   Block tag for state reads (default latest)
#   CHAIN_ID Expected chain id        (default 1337)
#
# Dependencies on $PATH: cast (foundry), yq (Mike Farah's Go yq v4+),
# xxd, awk, printf. All four are present by default on github-hosted
# ubuntu-latest runners + the bench-host base image.

set -euo pipefail

RPC=${RPC:-http://127.0.0.1:8545}
SPEC=${SPEC:-examples/spec-ci-comprehensive.yaml}
SEED=${SEED:-0}
MODE=${MODE:-pre}
BLOCK=${BLOCK:-latest}
CHAIN_ID=${CHAIN_ID:-1337}

if [[ "$MODE" != "pre" && "$MODE" != "post" ]]; then
    echo "FAIL: MODE must be 'pre' or 'post', got '$MODE'" >&2
    exit 2
fi

if [[ ! -f "$SPEC" ]]; then
    echo "FAIL: spec file not found: $SPEC" >&2
    exit 2
fi

# Tool sanity. shellcheck disable=SC2034
for tool in cast yq xxd awk printf; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "FAIL: required tool '$tool' not in PATH" >&2
        exit 2
    fi
done

GREEN=$'\033[0;32m'
RESET=$'\033[0m'
log_pass() { echo "${GREEN}PASS${RESET}: $*"; }

# fail prints the message and exits non-zero. Used inline; `set -e`
# would suffice for inherited failures, but explicit fail() makes the
# error message visible without the caller having to grep stderr.
fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# ============================================================================
# Address derivation — mirrors internal/specbuild/derive.go:13-34.
#
# Literal:  entity has `address:` → use as-is.
# Named:    entity has `name:` only → keccak256(BE_u64(seed) || utf8(name))[12:]
# Position: neither set         → keccak256(BE_u64(seed) || utf8("anon-N"))[12:]
# ============================================================================

SEED_BE_HEX=$(printf '%016x' "$SEED")

derive_addr() {
    # $1 = label (name OR "anon-N"). Returns 0x-prefixed 20-byte hex.
    local label="$1"
    local label_hex
    label_hex=$(printf '%s' "$label" | xxd -p -c 0)
    local k
    k=$(cast keccak "0x${SEED_BE_HEX}${label_hex}")
    # cast keccak returns 0x + 64 hex; last 40 chars = 20-byte address.
    echo "0x${k:26:40}"
}

resolve_entity_addr() {
    # $1 = entity index. Reads address/name from $SPEC via yq.
    local idx=$1
    local literal name
    literal=$(yq -r ".entities[$idx].address // \"\"" "$SPEC")
    name=$(yq -r ".entities[$idx].name // \"\"" "$SPEC")
    if [[ -n "$literal" ]]; then
        # Normalize: lowercase, with 0x prefix.
        echo "${literal,,}"
        return
    fi
    if [[ -n "$name" ]]; then
        derive_addr "$name"
        return
    fi
    derive_addr "anon-$idx"
}

# ============================================================================
# RPC helpers
# ============================================================================

rpc_balance() { cast balance "$1" --rpc-url "$RPC" --block "$BLOCK"; }
rpc_nonce()   { cast nonce   "$1" --rpc-url "$RPC" --block "$BLOCK"; }
rpc_code()    { cast code    "$1" --rpc-url "$RPC" --block "$BLOCK"; }
rpc_call()    { cast call    "$1" "$2" --rpc-url "$RPC" --block "$BLOCK"; }
rpc_storage() { cast storage "$1" "$2" --rpc-url "$RPC" --block "$BLOCK"; }

# ============================================================================
# Fixed assertions
# ============================================================================

check_chain_id() {
    local got want
    want="$CHAIN_ID"
    got=$(cast chain-id --rpc-url "$RPC")
    if [[ "$got" != "$want" ]]; then
        fail "chain-id: got $got, want $want"
    fi
    log_pass "chain-id == $want"
}

# Canonical system contracts deployed by syscontracts.AddCanonicalSystemContracts.
# All five must carry non-empty code at canonical mainnet addresses.
SYS_BEACON_ROOTS=0x000F3df6D732807Ef1319fB7B8bB8522d0Beac02
SYS_HISTORY_STORAGE=0x0000F90827F1C53a10cb7A02335B175320002935
SYS_WITHDRAWAL_QUEUE=0x00000961Ef480Eb55e80D19ad83579A64c007002
SYS_CONSOLIDATION_QUEUE=0x0000BBdDc7CE488642fb579F8B00f3a590007251
SYS_DEPOSIT_CONTRACT=0x00000000219ab540356cBB839Cbe05303d7705Fa

check_canonical_syscontracts() {
    for pair in \
        "BeaconRoots=$SYS_BEACON_ROOTS" \
        "HistoryStorage=$SYS_HISTORY_STORAGE" \
        "WithdrawalQueue=$SYS_WITHDRAWAL_QUEUE" \
        "ConsolidationQueue=$SYS_CONSOLIDATION_QUEUE" \
        "DepositContract=$SYS_DEPOSIT_CONTRACT"; do
        local label="${pair%%=*}"
        local addr="${pair##*=}"
        local code
        code=$(rpc_code "$addr")
        if [[ -z "$code" || "$code" == "0x" ]]; then
            fail "canonical syscontract $label at $addr: code missing"
        fi
        log_pass "canonical syscontract $label has code (${#code} chars)"
    done
}

# BEACON_ROOTS ring buffer (EIP-4788). After block 1 the pre-execution
# write should have populated slot (timestamp % 8191) with a non-zero
# parent-beacon-block-root. Only valid post-spamoor (block > 0).
check_beacon_roots_ring_buffer() {
    local ts slot val
    ts=$(cast block latest --rpc-url "$RPC" --field timestamp)
    if [[ -z "$ts" || "$ts" == "0" ]]; then
        fail "BEACON_ROOTS: latest block timestamp is zero/empty"
    fi
    slot=$((ts % 8191))
    val=$(rpc_storage "$SYS_BEACON_ROOTS" "$slot")
    if [[ -z "$val" || "$val" == "0x0000000000000000000000000000000000000000000000000000000000000000" ]]; then
        fail "BEACON_ROOTS slot $slot (= ts $ts %% 8191) is zero — EIP-4788 pre-exec didn't fire"
    fi
    log_pass "BEACON_ROOTS ring buffer slot $slot non-zero"
}

# Chain has advanced past genesis.
check_chain_advanced() {
    local bn
    bn=$(cast block-number --rpc-url "$RPC")
    if [[ -z "$bn" || "$bn" == "0" ]]; then
        fail "chain advance: block-number is 0 — chain didn't move"
    fi
    log_pass "chain advanced to block $bn"
}

# Spamoor sender nonce > 0 (post-spamoor only). The sender is whichever
# entity in the spec is named "spamoor-sender"; resolve it via yq.
check_spamoor_sender_nonce() {
    local sender_idx
    sender_idx=$(yq -r '.entities | to_entries | map(select(.value.name == "spamoor-sender")) | .[0].key // ""' "$SPEC")
    if [[ -z "$sender_idx" ]]; then
        fail "spec has no entity named 'spamoor-sender' — bench/CI cannot run spamoor"
    fi
    local addr nonce
    addr=$(resolve_entity_addr "$sender_idx")
    nonce=$(rpc_nonce "$addr")
    if [[ -z "$nonce" || "$nonce" == "0" ]]; then
        fail "spamoor sender $addr nonce is 0 post-spamoor (no txs submitted)"
    fi
    log_pass "spamoor sender $addr nonce = $nonce"
}

# ============================================================================
# Per-entity walk (spec-driven)
# ============================================================================

# Reads a balance value out of the spec — accepts decimal-string or 0x-hex.
# Echoes the decimal representation for cast balance comparison.
spec_balance_dec() {
    local raw="$1"
    if [[ -z "$raw" || "$raw" == "null" ]]; then
        echo "0"
        return
    fi
    if [[ "$raw" == 0x* || "$raw" == 0X* ]]; then
        printf '%d\n' "$raw"
    else
        # Strip surrounding quotes if yq left them.
        echo "${raw//\"/}"
    fi
}

check_entity() {
    local idx=$1
    local addr kind template name code_hex balance_raw nonce_want
    addr=$(resolve_entity_addr "$idx")
    kind=$(yq -r ".entities[$idx].kind" "$SPEC")
    template=$(yq -r ".entities[$idx].template // \"\"" "$SPEC")
    name=$(yq -r ".entities[$idx].name // \"\"" "$SPEC")
    code_hex=$(yq -r ".entities[$idx].code // \"\"" "$SPEC")
    balance_raw=$(yq -r ".entities[$idx].balance // \"\"" "$SPEC")
    nonce_want=$(yq -r ".entities[$idx].nonce // \"\"" "$SPEC")

    local label="entities[$idx]"
    if [[ -n "$name" ]]; then label="$label ($name)"; fi

    # Balance: when the spec sets an explicit value, assert byte-equality.
    # When unset, accept any value ≥ 0 (template-synthesized owners can
    # have non-zero balances even when the entity itself doesn't declare
    # one — and approximate_size_bytes-only entities get synthesized
    # storage, not balance).
    if [[ -n "$balance_raw" ]]; then
        local want got
        want=$(spec_balance_dec "$balance_raw")
        got=$(rpc_balance "$addr")
        if [[ "$got" != "$want" ]]; then
            fail "$label balance: got $got, want $want"
        fi
        log_pass "$label balance == $want"
    fi

    # Nonce: explicit-only assertion. State-actor floors ERC-20 to ≥1
    # (templates/erc20.go:203-206), so when the spec sets nonce on an
    # erc20 entity, the assertion still holds because the floor is 1.
    if [[ -n "$nonce_want" ]]; then
        local got
        got=$(rpc_nonce "$addr")
        if [[ "$got" != "$nonce_want" ]]; then
            fail "$label nonce: got $got, want $nonce_want"
        fi
        log_pass "$label nonce == $nonce_want"
    fi

    # Code:
    #  - explicit `code:` field        → byte-equal match.
    #  - kind: contract + template: erc20 → non-empty code (the runtime).
    #  - kind: contract + raw          → handled by the explicit-code arm above.
    if [[ -n "$code_hex" ]]; then
        local got
        got=$(rpc_code "$addr")
        # Lowercase + strip 0x for comparison.
        local got_norm="${got#0x}"
        got_norm="${got_norm,,}"
        local want_norm="${code_hex#0x}"
        want_norm="${want_norm,,}"
        if [[ "$got_norm" != "$want_norm" ]]; then
            fail "$label code mismatch (got ${#got_norm}/2 bytes, want ${#want_norm}/2)"
        fi
        log_pass "$label code matches (${#want_norm}/2 bytes)"
    elif [[ "$kind" == "contract" && "$template" == "erc20" ]]; then
        local got
        got=$(rpc_code "$addr")
        if [[ -z "$got" || "$got" == "0x" ]]; then
            fail "$label erc20 template: code empty (addr=$addr)"
        fi
        log_pass "$label erc20 code non-empty (${#got} chars, addr=$addr)"
    fi

    # ERC-20 template-specific assertions: name/symbol/decimals/totalSupply.
    if [[ "$kind" == "contract" && "$template" == "erc20" ]]; then
        local sym_want name_want dec_want
        sym_want=$(yq -r ".entities[$idx].parameters.symbol" "$SPEC")
        name_want=$(yq -r ".entities[$idx].parameters.name" "$SPEC")
        dec_want=$(yq -r ".entities[$idx].parameters.decimals // 18" "$SPEC")

        local got_name got_sym got_dec
        # cast call (string) returns the value wrapped in double quotes.
        got_name=$(rpc_call "$addr" "name()(string)" | tr -d '"')
        got_sym=$(rpc_call "$addr" "symbol()(string)" | tr -d '"')
        got_dec=$(rpc_call "$addr" "decimals()(uint8)")
        [[ "$got_name" == "$name_want" ]] || fail "$label erc20 name: got '$got_name', want '$name_want'"
        log_pass "$label erc20 name == '$name_want'"
        [[ "$got_sym" == "$sym_want" ]] || fail "$label erc20 symbol: got '$got_sym', want '$sym_want'"
        log_pass "$label erc20 symbol == '$sym_want'"
        [[ "$got_dec" == "$dec_want" ]] || fail "$label erc20 decimals: got '$got_dec', want '$dec_want'"
        log_pass "$label erc20 decimals == $dec_want"
        # totalSupply is queryable but its value is implementation-defined
        # (sum of synthesized owners + explicit owners). Assert ≥ 0
        # (always true; the call returning success is the real test).
        local got_total
        got_total=$(rpc_call "$addr" "totalSupply()(uint256)")
        [[ -n "$got_total" ]] || fail "$label erc20 totalSupply: empty"
        log_pass "$label erc20 totalSupply queryable (= $got_total)"
    fi
}

# ============================================================================
# Main
# ============================================================================

echo "═══════════════════════════════════════════════════════════════"
echo " verify-spec.sh  spec=$SPEC seed=$SEED mode=$MODE block=$BLOCK"
echo " rpc=$RPC"
echo "═══════════════════════════════════════════════════════════════"

check_chain_id
check_canonical_syscontracts

n_entities=$(yq -r '.entities | length' "$SPEC")
echo "  walking $n_entities spec entities…"
for ((i=0; i<n_entities; i++)); do
    check_entity "$i"
done

if [[ "$MODE" == "post" ]]; then
    check_chain_advanced
    check_beacon_roots_ring_buffer
    check_spamoor_sender_nonce
fi

echo "═══════════════════════════════════════════════════════════════"
echo "  verify-spec.sh: all checks passed (mode=$MODE)"
echo "═══════════════════════════════════════════════════════════════"
