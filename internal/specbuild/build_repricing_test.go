package specbuild

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestBuildRepricingMin loads examples/spec-repricing-min.yaml end-to-end
// (parse → validate → build) and asserts every new template emits the
// expected addresses. This is the canary that the example file, the
// docs (docs/SPEC.md), and the five repricing templates stay in sync.
//
// Counts here MUST equal the parameter values in the YAML; updating the
// example without updating this test (or vice versa) is a deliberate
// signal that the smoke contract has shifted.
func TestBuildRepricingMin(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-min.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	pre, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// sequential_eoas(count=100) + storage_pattern(1) +
	// create2_factory(1) + create2_deploys(salt_count=10) +
	// create_preimage_deploys(count=10) = 122.
	const want = 100 + 1 + 1 + 10 + 10
	if len(pre) != want {
		t.Fatalf("PreAlloc count: got %d, want %d", len(pre), want)
	}

	// Sequential EOAs: entries [0..100) should cover 0x1000..0x1063.
	for i := 0; i < 100; i++ {
		wantAddr := common.BigToAddress(common.Big1) // overwritten below
		wantAddr.SetBytes([]byte{0x10, byte(i)})
		var explicit common.Address
		explicit[18] = 0x10
		explicit[19] = byte(i)
		if pre[i].Address != explicit {
			t.Errorf("sequential_eoas[%d] addr: got %s, want %s",
				i, pre[i].Address.Hex(), explicit.Hex())
		}
		if pre[i].Code != nil {
			t.Errorf("sequential_eoas[%d] Code: must be nil for plain EOA", i)
		}
		if pre[i].Account.Nonce != 0 {
			t.Errorf("sequential_eoas[%d] nonce: got %d, want 0", i, pre[i].Account.Nonce)
		}
	}

	// Storage pattern (entry index 100): address 0x...c0de, slot 0 set.
	patAddr := common.HexToAddress("0x000000000000000000000000000000000000c0de")
	if pre[100].Address != patAddr {
		t.Errorf("storage_pattern addr: got %s, want %s",
			pre[100].Address.Hex(), patAddr.Hex())
	}
	if pre[100].Storage == nil {
		t.Errorf("storage_pattern: Storage iterator is nil")
	}
	if pre[100].Account.Nonce == 0 {
		t.Errorf("storage_pattern: nonce must default to >= 1 (got 0)")
	}

	// CREATE2 factory (entry 101): canonical address + canonical bytecode.
	if pre[101].Address != templates.CanonicalCREATE2FactoryAddress {
		t.Errorf("create2_factory addr: got %s, want %s",
			pre[101].Address.Hex(), templates.CanonicalCREATE2FactoryAddress.Hex())
	}
	if !bytes.Equal(pre[101].Code, templates.CanonicalCREATE2FactoryCode) {
		t.Errorf("create2_factory Code: does not match canonical runtime")
	}

	// CREATE2 deploys (entries 102..112): every derived address should
	// match crypto.CreateAddress2 from the YAML's initcode + factory.
	initcode := common.FromHex("0x6001600052602060006000F0")
	initHash := crypto.Keccak256(initcode)
	for i := 0; i < 10; i++ {
		var salt [32]byte
		// salt_start defaults to 0 in the example; salts are 0..9.
		salt[31] = byte(i)
		want := crypto.CreateAddress2(templates.CanonicalCREATE2FactoryAddress, salt, initHash)
		if pre[102+i].Address != want {
			t.Errorf("create2_deploys[%d] addr: got %s, want %s",
				i, pre[102+i].Address.Hex(), want.Hex())
		}
	}

	// CREATE-preimage deploys (entries 112..122): every derived address
	// should match crypto.CreateAddress(bittrex, 2+i) per the YAML.
	bittrex := common.HexToAddress("0xA3C1E324CA1CE40DB73ED6026C4A177F099B5770")
	for i := 0; i < 10; i++ {
		want := crypto.CreateAddress(bittrex, uint64(2+i))
		if pre[112+i].Address != want {
			t.Errorf("create_preimage_deploys[%d] addr: got %s, want %s",
				i, pre[112+i].Address.Hex(), want.Hex())
		}
	}
}
