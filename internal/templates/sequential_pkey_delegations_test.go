package templates

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// TestSequentialPkeyDelegationsMatchesEELS pins the authority addresses
// and full EIP-7702 delegation codes against execution-specs
// `yield_distinct_delegate_receiver()`: authority EOA(key=DELEGATE_BASE_KEY
// + i) delegates to EXISTING_CONTRACT_DIFF receiver i (max_diff CREATE2).
//
// start_pkey = keccak256("gas-repricings-7702-delegate"). Authorities and
// codes regenerated from an execution-specs checkout via
// account_sender_receiver.yield_distinct_delegate_receiver +
// AccountCreator(EXISTING_CONTRACT_DIFF).initcode.
func TestSequentialPkeyDelegationsMatchesEELS(t *testing.T) {
	ent := mkContractEntity("sequential_pkey_delegations", map[string]any{
		"start_pkey":   "0x959a83d905ff1fab43bf72c3e87020e4c77fd4bde0e5eeb48e5edbf74a9ec64e",
		"code_pattern": CodePatternMaxDiffPreAmsterdam,
		"count":        2,
		"balance":      "1000000000000000000",
	})
	out, err := (&sequentialPkeyDelegationsTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("count: got %d want 2", len(out))
	}
	type want struct {
		authority string
		code      string // full 23-byte 0xef0100||target
	}
	wants := []want{
		{"0xc700a71c3ee17f9106ff87e3a27dc52ce9d551eb", "0xef0100a9cb82ea3c688e8c8153e60c1e340840459f66a6"},
		{"0xf32d6227cde81dde6832accdb5288300f2f1c126", "0xef010099ea5fb3758b5fa20b4dfbd2971e98e9723f8f1f"},
	}
	bal := uint256.NewInt(1_000_000_000_000_000_000)
	for i, w := range wants {
		if out[i].Address != common.HexToAddress(w.authority) {
			t.Errorf("i=%d authority: got %s want EELS %s", i, out[i].Address.Hex(), w.authority)
		}
		gotCode := "0x" + common.Bytes2Hex(out[i].Code)
		if !strings.EqualFold(gotCode, w.code) {
			t.Errorf("i=%d code: got %s want %s", i, gotCode, w.code)
		}
		if out[i].Account.Nonce != 1 {
			t.Errorf("i=%d nonce: got %d want 1", i, out[i].Account.Nonce)
		}
		if out[i].Account.Balance.Cmp(bal) != 0 {
			t.Errorf("i=%d balance: got %s want 1e18", i, out[i].Account.Balance)
		}
	}
}

// TestSequentialPkeyDelegationsLiteralInitcode checks the literal-initcode
// target mode derives the same address create2_deploys would for that
// initcode (here the minimal STOP initcode).
func TestSequentialPkeyDelegationsLiteralInitcode(t *testing.T) {
	ent := mkContractEntity("sequential_pkey_delegations", map[string]any{
		"start_pkey": "0x959a83d905ff1fab43bf72c3e87020e4c77fd4bde0e5eeb48e5edbf74a9ec64e",
		"initcode":   "0x60016000f3", // minimal contract initcode
		"count":      1,
	})
	out, err := (&sequentialPkeyDelegationsTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// Target salt 0 for the minimal initcode (matches create2-minimal salt 0).
	wantTarget := common.HexToAddress("0x794a5Ba51C916E51aD4f45C85e67D9A07e8D0967")
	gotTarget := common.BytesToAddress(out[0].Code[3:]) // strip 0xef0100
	if gotTarget != wantTarget {
		t.Errorf("delegation target: got %s want %s", gotTarget.Hex(), wantTarget.Hex())
	}
}

// TestSequentialPkeyDelegationsValidation pins the parameter guards.
func TestSequentialPkeyDelegationsValidation(t *testing.T) {
	tmpl := sequentialPkeyDelegationsTemplate{}
	cases := []struct {
		name   string
		params map[string]any
		errSub string
	}{
		{"both target modes", map[string]any{
			"start_pkey": "0x959a83d905ff1fab43bf72c3e87020e4c77fd4bde0e5eeb48e5edbf74a9ec64e",
			"count":      1, "code_pattern": CodePatternMaxDiffPreAmsterdam, "initcode": "0x60016000f3",
		}, "exactly one"},
		{"no target", map[string]any{
			"start_pkey": "0x959a83d905ff1fab43bf72c3e87020e4c77fd4bde0e5eeb48e5edbf74a9ec64e", "count": 1,
		}, "missing delegation target"},
		{"unknown pattern", map[string]any{
			"start_pkey": "0x959a83d905ff1fab43bf72c3e87020e4c77fd4bde0e5eeb48e5edbf74a9ec64e",
			"count":      1, "code_pattern": "bogus",
		}, "unknown code_pattern"},
		{"short pkey", map[string]any{
			"start_pkey": "0x1234", "count": 1, "initcode": "0x60016000f3",
		}, "32 bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := tmpl.ValidateParameters(c.params)
			if err == nil || !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("want error containing %q, got %v", c.errSub, err)
			}
		})
	}
}
