package e2e_testing

import (
	"fmt"
	"testing"

	"github.com/ethereum/state-actor/internal/rpcprobe"
)

// AssertSpamoorOutputs verifies that spamoor's tx-blasting actually
// produced an observable state change on chain. Without this check, a
// client that accepts spamoor's payloads into blocks but silently drops
// EVM execution would still pass Phase 6 (which only asserts the
// deployer nonce ticked, not that anything actually executed).
//
// The check scans every block 1..latest for a contract-creation tx
// (whose `to` field is null), then asserts:
//   - the deploy tx's receipt has status 0x1 (i.e. the transaction did
//     not revert)
//   - the receipt's contractAddress has non-empty code at "latest"
//     (i.e. the EVM actually deployed the bytecode)
//
// Errors are reported via t.Errorf so a single CI run produces a useful
// diagnostic — caller continues running.
//
// Note on scan range: spamoor's wallet-funding phase scales with the
// configured wallet count and target gas, so the deploy can land
// anywhere from block 3 (small warmup) to block 100+ (large pre-spamoor
// state). Scanning to `latest` instead of a fixed cap keeps the check
// resilient.
func AssertSpamoorOutputs(t *testing.T, rpcURL string) {
	t.Helper()

	latest, err := rpcprobe.EthBlockNumber(rpcURL)
	if err != nil {
		t.Errorf("AssertSpamoorOutputs: eth_blockNumber: %v", err)
		return
	}

	for n := uint64(1); n <= latest; n++ {
		tag := fmt.Sprintf("0x%x", n)
		block, err := rpcprobe.BlockByNumberWithTxs(rpcURL, tag)
		if err != nil {
			t.Errorf("AssertSpamoorOutputs: %s: %v", tag, err)
			return
		}
		for _, tx := range block.Transactions {
			if tx.To != nil {
				continue
			}
			// Contract creation tx — fetch receipt for status + the
			// authoritative deploy address (avoids re-deriving with
			// crypto.CreateAddress, which would require parsing tx.Nonce).
			receipt, err := rpcprobe.EthGetTransactionReceipt(rpcURL, tx.Hash)
			if err != nil {
				t.Errorf("AssertSpamoorOutputs: receipt for %s: %v", tx.Hash.Hex(), err)
				return
			}
			if receipt.Status != "0x1" {
				t.Errorf("AssertSpamoorOutputs: deploy tx %s reverted (status=%q)", tx.Hash.Hex(), receipt.Status)
				return
			}
			if receipt.ContractAddress == nil {
				t.Errorf("AssertSpamoorOutputs: deploy receipt %s has nil contractAddress", tx.Hash.Hex())
				return
			}
			code, err := rpcprobe.EthGetCode(rpcURL, *receipt.ContractAddress, "latest")
			if err != nil {
				t.Errorf("AssertSpamoorOutputs: eth_getCode(%s): %v", receipt.ContractAddress.Hex(), err)
				return
			}
			if len(code) == 0 {
				t.Errorf("AssertSpamoorOutputs: deployed contract %s has empty code (EVM silently dropped execution?)", receipt.ContractAddress.Hex())
				return
			}
			t.Logf("AssertSpamoorOutputs: verified deployed contract %s (%d code bytes) in block %d", receipt.ContractAddress.Hex(), len(code), n)
			return
		}
	}
	t.Errorf("AssertSpamoorOutputs: no contract-creation tx found in blocks 1..%d (spamoor didn't deploy?)", latest)
}
