package e2e_testing

import (
	"strings"
	"testing"

	"github.com/ethereum/state-actor/internal/oracle"
)

// TestCISpecMatchesSpamoorSender asserts the spamoor sender entity in
// each canonical CI fixture uses the exact address constant from
// internal/oracle/devkeys.go. The YAML and the constant must stay in
// sync or spamoor will sign txs from an unfunded address and the CI
// suite will fail mysteriously at Phase 5.
//
// Runs in the default CI job (no build tags) so PRs that update one
// without the other surface immediately.
func TestCISpecMatchesSpamoorSender(t *testing.T) {
	for _, yamlPath := range []string{
		"../../examples/full-matrix-spec-feature.yaml",
	} {
		t.Run(yamlPath, func(t *testing.T) {
			preAlloc := LoadCISpecPreAlloc(t, yamlPath, "geth")
			wantAddr := oracle.SpamoorSenderAddr
			for _, pe := range preAlloc {
				if pe.Address == wantAddr {
					// Sanity: balance has 18-zero tail (≈1 ETH min).
					balStr := pe.Account.Balance.String()
					if !strings.HasSuffix(balStr, "000000000000000000") {
						t.Errorf("spamoor sender balance %s lacks 18-zero tail (likely under-funded)", balStr)
					}
					return
				}
			}
			t.Fatalf("%s has no entity at oracle.SpamoorSenderAddr (%s); "+
				"the YAML and devkeys.go drifted apart. Restore the entity or update the YAML.",
				yamlPath, wantAddr.Hex())
		})
	}
}
