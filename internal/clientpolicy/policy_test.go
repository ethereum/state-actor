package clientpolicy

import (
	"strings"
	"testing"
)

func TestValidateForClient_RecognizedClients(t *testing.T) {
	for _, c := range []string{"geth", "nethermind", "besu", "reth", "ethrex"} {
		if err := ValidateForClient(c, FlagValues{}); err != nil {
			t.Errorf("ValidateForClient(%q, zero FV): unexpected error: %v", c, err)
		}
	}
}

func TestValidateForClient_ErigonNotImplemented(t *testing.T) {
	err := ValidateForClient("erigon", FlagValues{})
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("expected 'not yet implemented' for erigon, got %v", err)
	}
}

func TestValidateForClient_UnknownRejected(t *testing.T) {
	err := ValidateForClient("frontier-of-1995", FlagValues{})
	if err == nil || !strings.Contains(err.Error(), "is not recognized") {
		t.Fatalf("expected 'not recognized' for unknown client, got %v", err)
	}
}

func TestValidateForClient_BinaryTrieGethOnly(t *testing.T) {
	if err := ValidateForClient("geth", FlagValues{BinaryTrie: true}); err != nil {
		t.Errorf("geth + --binary-trie should be allowed: %v", err)
	}
	for _, c := range []string{"nethermind", "besu", "reth", "ethrex"} {
		err := ValidateForClient(c, FlagValues{BinaryTrie: true})
		if err == nil || !strings.Contains(err.Error(), "EIP-7864") {
			t.Errorf("%s + --binary-trie should reject with EIP-7864 reason, got %v", c, err)
		}
	}
}

// TestValidateForClient_TargetSizeAllowed verifies --target-size is honored
// by every client after state-actor#54 added per-batch dirSize caps to
// nethermind (Phase 2) and reth (per-batch). Previously reth rejected the
// flag at parse time; that rejection is gone.
func TestValidateForClient_TargetSizeAllowed(t *testing.T) {
	for _, c := range []string{"geth", "nethermind", "besu", "reth", "ethrex"} {
		if err := ValidateForClient(c, FlagValues{TargetSize: "5GB"}); err != nil {
			t.Errorf("%s + --target-size should be allowed: %v", c, err)
		}
	}
}

func TestValidateForClient_ForkCeiling(t *testing.T) {
	// Pre-Prague forks are EOL and rejected globally by
	// genesis.BuildChainConfigForFork, so clientpolicy never sees them
	// in practice. The per-client ceiling check still matters for
	// future forks past osaka.
	cases := []struct {
		client   string
		fork     string
		wantPass bool
	}{
		{"geth", "prague", true},
		{"geth", "osaka", true},
		{"reth", "prague", true},
		{"reth", "osaka", true},
		{"besu", "prague", true},
		{"besu", "osaka", true},
		{"nethermind", "prague", true},
		{"nethermind", "osaka", true},
		{"ethrex", "prague", true},
		{"ethrex", "osaka", true},
	}
	for _, tc := range cases {
		err := ValidateForClient(tc.client, FlagValues{Fork: tc.fork})
		if tc.wantPass && err != nil {
			t.Errorf("%s + --fork=%s: expected pass, got %v", tc.client, tc.fork, err)
		}
		if !tc.wantPass && err == nil {
			t.Errorf("%s + --fork=%s: expected reject (past ceiling), got pass", tc.client, tc.fork)
		}
	}
}
