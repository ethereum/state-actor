package specbuild

import (
	"bytes"
	"strings"
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
	// match crypto.CreateAddress2 from the pattern's vendored initcode.
	// The YAML's create2_deploys entry uses
	// `code_pattern: unique_jumpdest_pre_amsterdam`, which owns both
	// initcode (constant, hashed for CREATE2 derivation) and the
	// per-derived-address runtime (24 KiB with embedded address).
	initcode := templates.BuildUniqueJumpdestInitcodePreAmsterdam()
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
		// And the planted runtime should be the 24 KiB unique-jumpdest
		// blob with the derived contract's own address embedded.
		if len(pre[102+i].Code) != 0x6000 {
			t.Errorf("create2_deploys[%d] runtime length: got %d, want 0x6000",
				i, len(pre[102+i].Code))
		}
		embedded := common.BytesToAddress(pre[102+i].Code[0x2C:0x40])
		if embedded != pre[102+i].Address {
			t.Errorf("create2_deploys[%d] embedded address mismatch: got %s in runtime, want %s (derived)",
				i, embedded.Hex(), pre[102+i].Address.Hex())
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

// TestBuildRepricingMinDeterministic pins the strongest contract the
// repricing templates must uphold: same YAML + same seed → byte-
// identical PreAlloc across runs, including every storage slot of the
// storage_pattern entity. The same guarantee is what makes the
// cross-client-genesis-root aggregator's invariant work.
//
// Modeled on TestBuildDeterminismEndToEnd; specialized to the
// repricing example because the bloater templates have their own
// fan-out paths (sequential_eoas, create2_deploys,
// create_preimage_deploys) that don't run through any other test's
// determinism scaffolding.
func TestBuildRepricingMinDeterministic(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-min.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	a, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build (run A): %v", err)
	}
	b, _, err := Build(s, defaultOpts)
	if err != nil {
		t.Fatalf("Build (run B): %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("entity count differs across runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Address != b[i].Address {
			t.Errorf("entity[%d] address: %s vs %s", i, a[i].Address.Hex(), b[i].Address.Hex())
		}
		if a[i].Account.Nonce != b[i].Account.Nonce {
			t.Errorf("entity[%d] nonce: %d vs %d", i, a[i].Account.Nonce, b[i].Account.Nonce)
		}
		if !a[i].Account.Balance.Eq(b[i].Account.Balance) {
			t.Errorf("entity[%d] balance: %s vs %s", i, a[i].Account.Balance, b[i].Account.Balance)
		}
		if !bytes.Equal(a[i].Account.CodeHash, b[i].Account.CodeHash) {
			t.Errorf("entity[%d] CodeHash differs", i)
		}
		if !bytes.Equal(a[i].Code, b[i].Code) {
			t.Errorf("entity[%d] Code bytes differ", i)
		}
		ma := drainStorage(a[i].Storage)
		mb := drainStorage(b[i].Storage)
		if len(ma) != len(mb) {
			t.Errorf("entity[%d] storage slot count: %d vs %d", i, len(ma), len(mb))
			continue
		}
		for k, va := range ma {
			vb, ok := mb[k]
			if !ok {
				t.Errorf("entity[%d] key %s missing in run B", i, k.Hex())
				continue
			}
			if va != vb {
				t.Errorf("entity[%d] key %s: %s vs %s", i, k.Hex(), va.Hex(), vb.Hex())
			}
		}
	}
}

// TestBuildRepricingMinCrossClient pins that the repricing templates'
// emitted addresses are independent of the client name in
// BuildOptions. This matters because the cross-client-genesis-root
// invariant assumes that for a given YAML + seed, every client sees
// the same set of addresses (sizecal calibration only varies the slot
// counts of approximate_size_bytes-driven entities, none of which are
// in the repricing example).
func TestBuildRepricingMinCrossClient(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-min.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	clients := []string{"geth", "besu", "nethermind", "reth"}
	var reference []common.Address
	for _, c := range clients {
		opts := defaultOpts
		opts.ClientName = c
		pre, _, err := Build(s, opts)
		if err != nil {
			t.Fatalf("Build(%s): %v", c, err)
		}
		addrs := make([]common.Address, len(pre))
		for i, pe := range pre {
			addrs[i] = pe.Address
		}
		if reference == nil {
			reference = addrs
			continue
		}
		if len(addrs) != len(reference) {
			t.Errorf("client=%s: PreAlloc length %d, want %d", c, len(addrs), len(reference))
			continue
		}
		for i, a := range addrs {
			if a != reference[i] {
				t.Errorf("client=%s entity[%d]: %s, want %s", c, i, a.Hex(), reference[i].Hex())
			}
		}
	}
}

// TestBuildRepricingCollision pins that specbuild.Build catches a
// fan-out template emitting addresses that collide with an earlier
// entity's address (or another fan-out's range). Two sequential_eoas
// entities whose ranges intersect must error rather than silently
// drop one of the overlapping accounts.
func TestBuildRepricingCollision(t *testing.T) {
	yaml := `entities:
  - kind: contract
    template: sequential_eoas
    name: range-a
    address: 0x0000000000000000000000000000000000001000
    parameters:
      count: 10
  - kind: contract
    template: sequential_eoas
    name: range-b
    address: 0x0000000000000000000000000000000000001005
    parameters:
      count: 10
`
	s, err := spec.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_, _, err = Build(s, defaultOpts)
	if err == nil {
		t.Fatalf("Build: expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("Build: error %q did not mention collision", err.Error())
	}
}
