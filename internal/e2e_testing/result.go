package e2e_testing

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"
)

// SuiteResult captures the per-client TestE2ESuite output the cross-client
// aggregator job downloads + compares. JSON-marshalled to $RESULT_PATH at
// the end of each suite test (or per phase, on partial failure — caller
// chooses).
//
// Fields are intentionally minimal — the aggregator only reads
// ClientName + GenesisStateRoot for the canonical-MPT cross-check. The
// other fields are kept for human debug logs and possible future
// per-phase assertions in CI.
type SuiteResult struct {
	// ClientName is one of "besu" / "geth" / "nethermind" / "reth".
	ClientName string `json:"client"`

	// GenesisStateRoot is eth_getBlockByNumber("0x0").stateRoot from the
	// running client. Cross-client aggregator requires this be identical
	// for the canonical-MPT seed (12345, 10 EOAs, 5 contracts, PowerLaw,
	// 1..100 slots, 256-byte code).
	GenesisStateRoot common.Hash `json:"genesis_state_root"`

	// PostSpamoorBlock is the chain tip number after the spamoor phase
	// SIGTERMed — proves the node accepted + processed blocks. Sanity:
	// must be ≥ TargetBlockDelta from the suite (~100).
	PostSpamoorBlock uint64 `json:"post_spamoor_block"`

	// PostSpamoorEntityCheck — "ok" if every entitygen-injected entity
	// (EOA balance / contract code / contract storage slot) was still
	// readable at "latest" with the same value as at "0x0", else a
	// short failure-cause string. Set by the suite test.
	PostSpamoorEntityCheck string `json:"post_spamoor_entity_check,omitempty"`

	// PostSpamoorDeployerNonce — nonce of spamoor's deployer wallet at
	// "latest". Non-zero proves spamoor actually sent transactions.
	// Set by the suite test.
	PostSpamoorDeployerNonce uint64 `json:"post_spamoor_deployer_nonce,omitempty"`

	// PostSpamoorChainAdvanced — true if eth_blockNumber > 0 after spamoor.
	// Failure here is recorded as t.Errorf, but the aggregator-visible
	// signal lives in this field so a pruned test-log doesn't hide it.
	PostSpamoorChainAdvanced bool `json:"post_spamoor_chain_advanced"`

	// PostSpamoorBeaconRootsOK — true if the EIP-4788 ring-buffer slot at
	// (latest.timestamp %% 8191) is non-zero (proves the pre-execution
	// actually ran). Same aggregator-visibility rationale as above.
	PostSpamoorBeaconRootsOK bool `json:"post_spamoor_beacon_roots_ok"`
}

// WriteResult marshals r to $RESULT_PATH. If RESULT_PATH is unset,
// logs a one-line stderr warning and returns nil (local runs without
// artifact upload — the warning surfaces visibly so a dev who skipped
// the `make` wrapper sees their suite isn't producing the artifact CI
// would consume). Errors during write are returned — the suite test
// should t.Fatalf on them so CI uploads don't get stale results.
func WriteResult(r SuiteResult) error {
	path := os.Getenv("RESULT_PATH")
	if path == "" {
		fmt.Fprintln(os.Stderr, "oracle: RESULT_PATH unset; skipping artifact write (set RESULT_PATH=/tmp/result.json or use `make test-{client}-suite`)")
		return nil
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal SuiteResult: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
