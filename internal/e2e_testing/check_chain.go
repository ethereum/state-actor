package e2e_testing

import (
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"

	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// CheckChainID asserts the running client returns `want` for eth_chainId.
// Catches misconfigured genesis chain-id at the per-client boot layer
// (e.g. besu/neth config file drift from the synthesized genesis).
func CheckChainID(t *testing.T, rpcURL string, want uint64) bool {
	t.Helper()
	got, err := rpcprobe.EthChainID(rpcURL)
	if err != nil {
		t.Errorf("eth_chainId: %v", err)
		return false
	}
	if got != want {
		t.Errorf("eth_chainId: got %d, want %d", got, want)
		return false
	}
	return true
}

// canonicalSyscontracts is the 5-element fixed list every CI client
// genesis must carry per syscontracts.AddCanonicalSystemContracts.
var canonicalSyscontracts = []struct {
	label string
	addr  common.Address
}{
	{"BeaconRoots", params.BeaconRootsAddress},
	{"HistoryStorage", params.HistoryStorageAddress},
	{"WithdrawalQueue", params.WithdrawalQueueAddress},
	{"ConsolidationQueue", params.ConsolidationQueueAddress},
	{"DepositContract", syscontracts.DepositContractAddress},
}

// CheckCanonicalSyscontracts asserts every canonical mainnet system
// contract has code on chain. Used by Phase 4 (block 0) — the
// canonical 5 are injected at genesis by every supported client.
func CheckCanonicalSyscontracts(t *testing.T, rpcURL, blockTag string) bool {
	t.Helper()
	passed := true
	for _, c := range canonicalSyscontracts {
		code, err := rpcprobe.EthGetCode(rpcURL, c.addr, blockTag)
		if err != nil {
			t.Errorf("[%s] canonical syscontract %s eth_getCode %s: %v",
				blockTag, c.label, c.addr.Hex(), err)
			passed = false
			continue
		}
		if len(code) == 0 {
			t.Errorf("[%s] canonical syscontract %s at %s: code missing",
				blockTag, c.label, c.addr.Hex())
			passed = false
		}
	}
	return passed
}

// CheckChainAdvanced asserts block-number > 0. Used post-spamoor as a
// liveness check: spamoor sent txs AND the client mined at least one
// block including them. Cheaper than walking receipts.
func CheckChainAdvanced(t *testing.T, rpcURL string) bool {
	t.Helper()
	n, err := rpcprobe.EthBlockNumber(rpcURL)
	if err != nil {
		t.Errorf("eth_blockNumber: %v", err)
		return false
	}
	if n == 0 {
		t.Errorf("chain advance: block-number is 0 — chain didn't move post-spamoor")
		return false
	}
	return true
}

// CheckBeaconRootsRingBuffer asserts the EIP-4788 ring-buffer slot at
// (latest.timestamp % 8191) is non-zero — proves the BeaconRoots
// pre-execution actually wrote on the latest block. Post-spamoor only.
//
// EIP-4788's BeaconRoots contract writes TWO slots per system call:
//   - slot `ts % 8191`        ← the block timestamp itself
//   - slot `(ts % 8191) + 8191` ← the parent beacon block root
//
// We read the FIRST slot (timestamp). In `--dev` mode there is no real
// consensus client, so the parent-beacon-block-root slot is often a
// literal zero hash — reading it would give false negatives. The
// timestamp slot is non-zero whenever block.timestamp > 0, which is
// the actual "the pre-exec ran" signal we want.
func CheckBeaconRootsRingBuffer(t *testing.T, rpcURL string) bool {
	t.Helper()
	const beaconRootsBufferLen uint64 = 8191
	blk, err := rpcprobe.BlockByNumber(rpcURL, "latest")
	if err != nil {
		t.Errorf("BlockByNumber(latest): %v", err)
		return false
	}
	tsHex := strings.TrimPrefix(blk.Timestamp, "0x")
	// "" / "0" both produce ts=0 → slot=0, which is meaningless. Reject
	// both with a message that disambiguates "missing field" from
	// "block has literal zero timestamp."
	if tsHex == "" || tsHex == "0" {
		t.Errorf("BEACON_ROOTS: latest block timestamp is missing or literal zero (raw=%q) — chain may not have produced a non-genesis block",
			blk.Timestamp)
		return false
	}
	ts, err := strconv.ParseUint(tsHex, 16, 64)
	if err != nil {
		t.Errorf("BEACON_ROOTS: parse timestamp %q: %v", blk.Timestamp, err)
		return false
	}
	slot := ts % beaconRootsBufferLen
	slotHash := common.BigToHash(new(big.Int).SetUint64(slot))
	val, err := rpcprobe.EthGetStorageAt(rpcURL, params.BeaconRootsAddress, slotHash, "latest")
	if err != nil {
		t.Errorf("BEACON_ROOTS eth_getStorageAt slot %d: %v", slot, err)
		return false
	}
	if val == (common.Hash{}) {
		t.Errorf("BEACON_ROOTS slot %d (= ts %d %% %d) is zero — EIP-4788 pre-exec didn't fire",
			slot, ts, beaconRootsBufferLen)
		return false
	}
	return true
}
