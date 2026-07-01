package autofill

import (
	"testing"

	"github.com/ethereum/state-actor/internal/sizecal"
)

func TestParseProfile(t *testing.T) {
	cases := map[string]Profile{
		"":         ProfileMainnet,
		"mainnet":  ProfileMainnet,
		"accounts": ProfileAccounts,
	}
	for in, want := range cases {
		got, err := ParseProfile(in)
		if err != nil {
			t.Fatalf("ParseProfile(%q): unexpected err %v", in, err)
		}
		if got != want {
			t.Errorf("ParseProfile(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseProfile("garbage"); err == nil {
		t.Error("ParseProfile(garbage) should error")
	}
}

// TestProfileAccounts_PureEOAs confirms the accounts profile routes the
// whole budget into EOAs (no contracts, no storage) so the trie is
// account-dominated and stays on the streaming DrawEOA path.
func TestProfileAccounts_PureEOAs(t *testing.T) {
	const budget = 10 << 30 // 10 GiB
	p, err := PlanForBudgetProfile(budget, ProfileAccounts)
	if err != nil {
		t.Fatalf("PlanForBudgetProfile(accounts): %v", err)
	}
	if p.NumContracts != 0 {
		t.Errorf("accounts profile produced %d contracts, want 0", p.NumContracts)
	}
	if p.NumEOAs <= 0 {
		t.Fatalf("accounts profile produced %d EOAs, want > 0", p.NumEOAs)
	}
	// The whole budget is account-trie, so EOAs ≈ budget / bytesPerAccount.
	wantEOAs := budget / uint64(sizecal.BytesPerAccount(""))
	if got := uint64(p.NumEOAs); got != wantEOAs {
		t.Errorf("accounts EOAs = %d, want %d (= budget/bytesPerAccount)", got, wantEOAs)
	}
}

// TestProfileMainnet_BackwardCompat confirms the default profile is
// byte-for-byte the same plan PlanForBudget always produced (contracts +
// storage present), so existing callers are unaffected.
func TestProfileMainnet_BackwardCompat(t *testing.T) {
	const budget = 10 << 30
	legacy, err := PlanForBudget(budget)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	viaProfile, err := PlanForBudgetProfile(budget, ProfileMainnet)
	if err != nil {
		t.Fatalf("PlanForBudgetProfile(mainnet): %v", err)
	}
	if *legacy != *viaProfile {
		t.Errorf("mainnet profile diverged from PlanForBudget:\n legacy=%+v\n profile=%+v", legacy, viaProfile)
	}
	if legacy.NumContracts == 0 {
		t.Error("mainnet profile unexpectedly produced 0 contracts at 10 GiB")
	}
}
