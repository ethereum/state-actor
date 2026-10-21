package clientpolicy

import (
	"strings"
	"testing"
)

func TestValidateForClient_RecognizedClients(t *testing.T) {
	for _, c := range []string{"geth", "nethermind", "besu", "reth", "erigon", "ethrex", "nimbus"} {
		if err := ValidateForClient(c, FlagValues{}); err != nil {
			t.Errorf("ValidateForClient(%q, zero FV): unexpected error: %v", c, err)
		}
	}
}

// TestValidateForClient_ErigonBinaryTrieRejected replaces the prior
// TestValidateForClient_ErigonNotImplemented now that the erigon dispatch
// arm is wired (it returns errNotImplemented at runtime through the
// !cgo_erigon stub, but the recognition layer accepts it). The flag-level
// policy still rejects --binary-trie for erigon per the EIP-7864 absence.
func TestValidateForClient_ErigonBinaryTrieRejected(t *testing.T) {
	err := ValidateForClient("erigon", FlagValues{BinaryTrie: true})
	if err == nil || !strings.Contains(err.Error(), "EIP-7864") {
		t.Fatalf("expected EIP-7864 rejection for erigon + --binary-trie, got %v", err)
	}
}

func TestValidateForClient_UnknownRejected(t *testing.T) {
	err := ValidateForClient("frontier-of-1995", FlagValues{})
	if err == nil || !strings.Contains(err.Error(), "is not recognized") {
		t.Fatalf("expected 'not recognized' for unknown client, got %v", err)
	}
	// The "valid values" hint must list every recognized client so users
	// can typo-correct without re-reading the docs. Verifies erigon is
	// included alongside the other four.
	for _, c := range []string{"erigon", "nimbus"} {
		if !strings.Contains(err.Error(), c) {
			t.Fatalf("expected 'valid values' hint to include %q, got %v", c, err)
		}
	}
}

func TestValidateForClient_BinaryTrieGethOnly(t *testing.T) {
	for _, m := range []string{"", "normal"} {
		if err := ValidateForClient("geth", FlagValues{BinaryTrie: true, StorageModel: m}); err != nil {
			t.Errorf("geth + --binary-trie + --storage-model=%q should be allowed: %v", m, err)
		}
	}
	if err := ValidateForClient("geth", FlagValues{BinaryTrie: true, StorageModel: "mainnet"}); err == nil {
		t.Error("geth + --binary-trie + --storage-model=mainnet should be rejected")
	}
	if err := ValidateForClient("geth", FlagValues{BinaryTrie: true, AccountModel: "mainnet"}); err == nil {
		t.Error("geth + --binary-trie + --account-model=mainnet should be rejected")
	}
	for _, c := range []string{"nethermind", "besu", "reth", "erigon", "ethrex", "nimbus"} {
		err := ValidateForClient(c, FlagValues{BinaryTrie: true})
		if err == nil || !strings.Contains(err.Error(), "EIP-7864") {
			t.Errorf("%s + --binary-trie should reject with EIP-7864 reason, got %v", c, err)
		}
	}
}

func TestValidateForClient_MdbxFillRethOnly(t *testing.T) {
	if err := ValidateForClient("reth", FlagValues{MdbxFillProfile: "migrated"}); err != nil {
		t.Errorf("reth + --mdbx-fill-profile=migrated should be allowed: %v", err)
	}
	if err := ValidateForClient("reth", FlagValues{MdbxFillOverridden: true}); err != nil {
		t.Errorf("reth + --mdbx-fill overrides should be allowed: %v", err)
	}
	for _, c := range []string{"geth", "nethermind", "besu", "erigon", "ethrex", "nimbus"} {
		if err := ValidateForClient(c, FlagValues{MdbxFillProfile: "migrated"}); err == nil || !strings.Contains(err.Error(), "only supported with --client=reth") {
			t.Errorf("%s + --mdbx-fill-profile=migrated should reject with reth-only reason, got %v", c, err)
		}
		if err := ValidateForClient(c, FlagValues{MdbxFillOverridden: true}); err == nil || !strings.Contains(err.Error(), "only supported with --client=reth") {
			t.Errorf("%s + --mdbx-fill overrides should reject with reth-only reason, got %v", c, err)
		}
	}
	// "fresh" (the flag default) must be allowed everywhere, same as leaving
	// MdbxFillProfile unset.
	for _, c := range []string{"geth", "nethermind", "besu", "reth", "erigon", "ethrex", "nimbus"} {
		if err := ValidateForClient(c, FlagValues{MdbxFillProfile: "fresh"}); err != nil {
			t.Errorf("%s + --mdbx-fill-profile=fresh should be allowed: %v", c, err)
		}
	}
}

// TestValidateForClient_TargetSizeAllowed verifies --target-size is honored
// by every client after state-actor#54 added per-batch dirSize caps to
// nethermind (Phase 2) and reth (per-batch). Previously reth rejected the
// flag at parse time; that rejection is gone.
func TestValidateForClient_TargetSizeAllowed(t *testing.T) {
	for _, c := range []string{"geth", "nethermind", "besu", "reth", "erigon", "ethrex", "nimbus"} {
		if err := ValidateForClient(c, FlagValues{TargetSize: "5GB"}); err != nil {
			t.Errorf("%s + --target-size should be allowed: %v", c, err)
		}
	}
}

func TestValidateForClient_ForkCeiling(t *testing.T) {
	// Pre-Prague forks are EOL and rejected globally by
	// genesis.BuildChainConfigForFork, so clientpolicy never sees them
	// in practice. The per-client ceiling check matters for forks past
	// osaka: every client writes amsterdam.
	cases := []struct {
		client   string
		fork     string
		wantPass bool
	}{
		{"geth", "prague", true},
		{"geth", "osaka", true},
		{"geth", "amsterdam", true},
		{"reth", "prague", true},
		{"reth", "osaka", true},
		{"reth", "amsterdam", true},
		{"besu", "prague", true},
		{"besu", "osaka", true},
		{"besu", "amsterdam", true},
		{"nethermind", "prague", true},
		{"nethermind", "osaka", true},
		{"nethermind", "amsterdam", true},
		// Erigon's writer ceiling is osaka — verified empirically on the
		// bloatnet bench host that `erigon init` accepts a genesis.json
		// with OsakaTime active. Matches the other clients.
		{"erigon", "prague", true},
		{"erigon", "osaka", true},
		{"erigon", "amsterdam", true},
		{"ethrex", "prague", true},
		{"ethrex", "osaka", true},
		{"ethrex", "amsterdam", true},
		{"nimbus", "prague", true},
		{"nimbus", "osaka", true},
		{"nimbus", "amsterdam", true},
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
