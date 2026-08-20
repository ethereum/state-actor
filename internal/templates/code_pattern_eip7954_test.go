package templates

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// eip7954MaxCodeSize is the contract-code limit EIP-7954 raises EIP-170's
// 24576 to at Amsterdam, and the size the execution-specs
// `AccountMode.EXISTING_CONTRACT_*_64KIB` modes deploy at.
//
// Note that go-ethereum carried MaxCodeSizeAmsterdam = 32768 for a while
// before settling on 65536 (glamsterdam-devnet-8 has the latter, matching both
// EIP-7954 and EIP-7907). Nothing here depends on the client constant --
// state-actor writes the runtime into the database rather than deploying it --
// but a client still enforcing 32768 would reject these deployments, leaving
// the addresses below empty.
const eip7954MaxCodeSize = 0x10000

// TestUniqueJumpdestEIP7954AddressesMatchEEST pins the derived addresses for
// unique_jumpdest at the EIP-7954 limit, which the other adjustable patterns
// already have (TestCreate2DeploysMaxAdjustableEIP7954AddressesMatchEEST) and
// this one only had at 0x20000.
//
// This is the size execution-specs deploys at for
// AccountMode.EXISTING_CONTRACT_JUMPDEST_64KIB, so a divergence here points
// the benchmarks at accounts that were never deployed to -- which EXTCODESIZE
// answers without complaint. Regenerate from an execution-specs checkout:
//
//	from tests.benchmark.helper.account_creator import AccountCreator, AccountMode
//	from execution_testing import DETERMINISTIC_FACTORY_ADDRESS, compute_create2_address
//	ic = AccountCreator(AccountMode.EXISTING_CONTRACT_JUMPDEST_64KIB).initcode
//	print(compute_create2_address(address=DETERMINISTIC_FACTORY_ADDRESS, salt=0, initcode=ic))
func TestUniqueJumpdestEIP7954AddressesMatchEEST(t *testing.T) {
	ent := mkContractEntity("create2_deploys", map[string]any{
		"code_pattern": CodePatternUniqueJumpdest,
		"code_size":    eip7954MaxCodeSize,
		"salt_count":   2,
	})
	out, err := (&create2DeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []common.Address{
		common.HexToAddress("0x87bebfe2a4bf48bbf543aa1f2387248c2aa44349"), // salt 0
		common.HexToAddress("0xad115251826fe8345143820c43c74a5131723367"), // salt 1
	}
	if len(out) != len(want) {
		t.Fatalf("count: got %d, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("salt=%d: got %s, want EEST-derived %s",
				i, out[i].Address.Hex(), want[i].Hex())
		}
		if len(out[i].Code) != eip7954MaxCodeSize {
			t.Errorf("salt=%d: runtime length got %d, want %#x",
				i, len(out[i].Code), eip7954MaxCodeSize)
		}
		// The entry jump target 0xFFFF is the largest that still fits a
		// PUSH2, so this size keeps the 4-byte entry the default size has.
		entry := []byte{0x61, 0xFF, 0xFF, 0x56}
		for j, b := range entry {
			if out[i].Code[j] != b {
				t.Errorf("salt=%d: entry byte %d got 0x%02x, want 0x%02x",
					i, j, out[i].Code[j], b)
			}
		}
		embedded := common.BytesToAddress(out[i].Code[0x2C:0x40])
		if embedded != out[i].Address {
			t.Errorf("salt=%d: embedded address %s != derived %s",
				i, embedded.Hex(), out[i].Address.Hex())
		}
	}
	if out[0].Address == out[1].Address {
		t.Errorf("salts 0 and 1 derived the same address")
	}
}

// TestEIP7954InitcodeMatchesEEST pins keccak256 of all three adjustable
// patterns' initcode at the EIP-7954 limit in one place, so that the size the
// 64 KiB account modes use is covered for every pattern rather than for two of
// the three. The values were read back from the initcode execution-specs
// builds, which is byte-identical to what these builders emit at this size.
func TestEIP7954InitcodeMatchesEEST(t *testing.T) {
	for _, c := range []struct {
		name     string
		initcode []byte
		keccak   string
	}{
		{"max_same", BuildMaxSameInitcode(eip7954MaxCodeSize),
			"0x4b81bb2f80a8d129fb92d8efac6eeadab97b87b9d2434d2c67929b19a85caff8"},
		{"max_diff", BuildMaxDiffInitcode(eip7954MaxCodeSize),
			"0xfb051ac6360cad63b030de61ddcf46c4ee1c00c53cc9d28626c763c9f4763789"},
		{"unique_jumpdest", BuildUniqueJumpdestInitcode(eip7954MaxCodeSize),
			"0xc255e15a2e24a89fd585858f4b20b46fe8eab3ce309d994be18f343f539823bb"},
	} {
		if got := crypto.Keccak256Hash(c.initcode).Hex(); got != c.keccak {
			t.Errorf("%s: initcode keccak diverged from EEST: got %s want %s",
				c.name, got, c.keccak)
		}
	}
}
