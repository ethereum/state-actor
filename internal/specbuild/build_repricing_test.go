package specbuild

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestBuildRepricingSmoke loads examples/spec-repricing-smoke.yaml
// end-to-end (parse → validate → build) and asserts every new template
// emits the expected addresses. This is the canary that the example
// file, the docs (docs/SPEC.md), and the five repricing templates stay
// in sync.
//
// Counts here MUST equal the parameter values in the YAML; updating the
// example without updating this test (or vice versa) is a deliberate
// signal that the smoke contract has shifted. The "min"-sized
// production fixture is examples/spec-repricing-min.yaml; this test
// stays small so it can run in milliseconds.
func TestBuildRepricingSmoke(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-smoke.yaml")
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
	// create_preimage_deploys(count=10) + sequential_pkey_eoas(count=3)
	// = 125.
	const want = 100 + 1 + 1 + 10 + 10 + 3
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

	// Sender pool (entries 122..125): three plain EOAs at addresses
	// derived from pkeys 0x222…2, 0x222…2+1, 0x222…2+2. The pkey-
	// derived address is the secp256k1 PubkeyToAddress of the scalar
	// padded to 32 bytes big-endian. The template enforces nonce=0 +
	// no code; only the addresses themselves need checking.
	base := new(big.Int).SetBytes(common.FromHex(
		"0x2222222222222222222222222222222222222222222222222222222222222222",
	))
	for i := 0; i < 3; i++ {
		var buf [32]byte
		scalar := new(big.Int).Add(base, big.NewInt(int64(i)))
		b := scalar.Bytes()
		copy(buf[32-len(b):], b)
		priv, err := crypto.ToECDSA(buf[:])
		if err != nil {
			t.Fatalf("sender_pool[%d] control derive: %v", i, err)
		}
		wantAddr := crypto.PubkeyToAddress(priv.PublicKey)
		if pre[122+i].Address != wantAddr {
			t.Errorf("sequential_pkey_eoas[%d] addr: got %s, want %s",
				i, pre[122+i].Address.Hex(), wantAddr.Hex())
		}
		if pre[122+i].Code != nil {
			t.Errorf("sequential_pkey_eoas[%d] Code: must be nil for plain EOA", i)
		}
		if pre[122+i].Account.Nonce != 0 {
			t.Errorf("sequential_pkey_eoas[%d] nonce: got %d, want 0", i, pre[122+i].Account.Nonce)
		}
	}
}

// TestBuildRepricingSmokeDeterministic pins the strongest contract the
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
func TestBuildRepricingSmokeDeterministic(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-smoke.yaml")
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

// TestBuildRepricingSmokeCrossClient pins that the repricing templates'
// emitted addresses are independent of the client name in
// BuildOptions. This matters because the cross-client-genesis-root
// invariant assumes that for a given YAML + seed, every client sees
// the same set of addresses (sizecal calibration only varies the slot
// counts of approximate_size_bytes-driven entities, none of which are
// in the repricing example).
func TestBuildRepricingSmokeCrossClient(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-smoke.yaml")
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

// TestParseRepricingMin keeps the production-minimum fixture
// (`examples/spec-repricing-min.yaml`, 150 000 of each fan-out
// template) parseable and schema-valid in CI. We deliberately do NOT
// call Build here — the 150 000-entry create2_deploys entry would take
// minutes and produce ~3.7 GB of code in memory. The smoke fixture
// (TestBuildRepricingSmoke) is the end-to-end Build canary; this is
// just the lint canary for the larger sibling.
func TestParseRepricingMin(t *testing.T) {
	s, err := spec.ParseFile("../../examples/spec-repricing-min.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Cheap sanity: every fan-out count is at least 150 000. If a
	// future edit drops one below the 300 M-gas headroom threshold
	// the file's own header lies, so this catches the divergence.
	const need = uint64(150000)
	gotCount := func(name string, p map[string]any, key string) uint64 {
		v, ok := p[key]
		if !ok {
			t.Fatalf("entity %q: missing parameter %q", name, key)
		}
		switch x := v.(type) {
		case int:
			return uint64(x)
		case int64:
			return uint64(x)
		case uint64:
			return x
		case float64:
			return uint64(x)
		default:
			t.Fatalf("entity %q: %s has unexpected type %T", name, key, v)
			return 0
		}
	}
	checked := map[string]bool{
		"sequential-eoas-300m":         false,
		"storage-pattern-10gb":         false,
		"create2-deploys-300m":         false,
		"bittrex-create-preimage-300m": false,
		"sender-pool-300m":             false,
	}
	for _, ent := range s.Entities {
		switch ent.Name {
		case "storage-pattern-10gb":
			// storage_pattern is the cold-SLOAD/SSTORE knob; 150 000
			// populated slots is the same 300 M-gas / 2000 gas-per-access
			// floor as the other knobs. The fixture deliberately
			// oversizes it (the 10 GB variant), but the floor is what
			// benchmark validity requires — below it a 300 M-gas
			// iteration walks off the planted slot range.
			if c := gotCount(ent.Name, ent.Parameters, "final"); c < need {
				t.Errorf("%s final=%d, must be >= %d (300 M-gas headroom)", ent.Name, c, need)
			}
			checked[ent.Name] = true
		case "sequential-eoas-300m":
			if c := gotCount(ent.Name, ent.Parameters, "count"); c < need {
				t.Errorf("%s count=%d, must be >= %d (300 M-gas headroom)", ent.Name, c, need)
			}
			checked[ent.Name] = true
		case "create2-deploys-300m":
			if c := gotCount(ent.Name, ent.Parameters, "salt_count"); c < need {
				t.Errorf("%s salt_count=%d, must be >= %d (300 M-gas headroom)", ent.Name, c, need)
			}
			checked[ent.Name] = true
		case "bittrex-create-preimage-300m":
			if c := gotCount(ent.Name, ent.Parameters, "count"); c < need {
				t.Errorf("%s count=%d, must be >= %d (300 M-gas headroom)", ent.Name, c, need)
			}
			checked[ent.Name] = true
		case "sender-pool-300m":
			// sender_pool scales with iteration COUNT
			// (one-sender-per-transfer-tx), not gas-per-access.
			// 30 M-gas / 21 k-intrinsic = ~1428 senders consumed
			// per fill; 150 000 leaves the same scaling headroom
			// as the cold-access knobs.
			if c := gotCount(ent.Name, ent.Parameters, "count"); c < need {
				t.Errorf("%s count=%d, must be >= %d (300 M-gas headroom)", ent.Name, c, need)
			}
			checked[ent.Name] = true
		}
	}
	for name, ok := range checked {
		if !ok {
			t.Errorf("entity %q not found in spec-repricing-min.yaml (rename?)", name)
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
