package specbuild

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/spec"
	"github.com/ethereum/state-actor/internal/templates"
)

// TestBuildFullMatrixRepricingTemplates drives the six repricing-
// benchmark templates through the full-matrix CI fixture
// (examples/full-matrix-spec-feature.yaml, entities 23-29) and asserts
// each emits the addresses, code, and storage its parameters imply.
// This is the canary that the spec layer, the fixture, docs/SPEC.md,
// and the six templates stay in sync; the per-client TestE2ESuite then
// materializes the same entities and RPC-probes them.
//
// Lookups are by derived address (not absolute PreAlloc index) so the
// test is robust to entities being added earlier in the fixture. The
// full-matrix fixture covers a superset of the modes a dedicated
// repricing fixture would: both create2_deploys flavors (literal
// runtime in entity 25, code_pattern in entity 29), the 1-wei-default
// sequential_eoas (entity 27), and storage_init on code-bearing
// CREATE-preimage children (entity 26).
func TestBuildFullMatrixRepricingTemplates(t *testing.T) {
	s, err := spec.ParseFile("../../examples/full-matrix-spec-feature.yaml")
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
	idx := make(map[common.Address]int, len(pre))
	for i := range pre {
		idx[pre[i].Address] = i
	}
	get := func(label string, a common.Address) int {
		j, ok := idx[a]
		if !ok {
			t.Fatalf("%s: address %s not found in PreAlloc", label, a.Hex())
		}
		return j
	}

	// Entity 23 — storage_pattern (final=20, nonce=42). Slot 0 holds the
	// next-free pointer (final+1 = 21); slots 1..20 are the identity map.
	{
		j := get("storage_pattern", common.HexToAddress("0x000000000000000000000000000000000000c0de"))
		if pre[j].Account.Nonce != 42 {
			t.Errorf("storage_pattern nonce: got %d, want 42", pre[j].Account.Nonce)
		}
		slots := drainStorage(pre[j].Storage)
		if len(slots) != 21 {
			t.Errorf("storage_pattern slot count: got %d, want 21 (final+1)", len(slots))
		}
		var want0 common.Hash
		want0[31] = 21
		if got := slots[common.Hash{}]; got != want0 {
			t.Errorf("storage_pattern slot 0: got %s, want %s (final+1)", got.Hex(), want0.Hex())
		}
	}

	// Entity 24 — create2_factory: canonical address + canonical runtime.
	{
		j := get("create2_factory", templates.CanonicalCREATE2FactoryAddress)
		if !bytes.Equal(pre[j].Code, templates.CanonicalCREATE2FactoryCode) {
			t.Errorf("create2_factory Code: does not match canonical runtime")
		}
	}

	// Entity 25 — create2_deploys, LITERAL runtime mode (salt_count=3).
	// Each derived address is crypto.CreateAddress2(factory, salt,
	// keccak(initcode)); every derived contract carries the literal
	// runtime verbatim.
	{
		initcode := common.FromHex("0x6001600052602060006000F0")
		initHash := crypto.Keccak256(initcode)
		runtime := common.FromHex("0x615fff565b")
		for i := 0; i < 3; i++ {
			var salt [32]byte
			salt[31] = byte(i)
			addr := crypto.CreateAddress2(templates.CanonicalCREATE2FactoryAddress, salt, initHash)
			j := get("create2_deploys[literal]", addr)
			if !bytes.Equal(pre[j].Code, runtime) {
				t.Errorf("create2_deploys[literal][%d] Code: got %x, want %x", i, pre[j].Code, runtime)
			}
		}
	}

	// Entity 29 — create2_deploys, code_pattern mode (salt_count=2). The
	// pattern owns both the initcode (hashed for CREATE2 derivation) and
	// a 24 KiB per-address-unique runtime with the contract's own address
	// embedded at bytes 0x2C..0x40.
	{
		initcode := templates.BuildUniqueJumpdestInitcodePreAmsterdam()
		initHash := crypto.Keccak256(initcode)
		for i := 0; i < 2; i++ {
			var salt [32]byte
			salt[31] = byte(i)
			addr := crypto.CreateAddress2(templates.CanonicalCREATE2FactoryAddress, salt, initHash)
			j := get("create2_deploys[pattern]", addr)
			if len(pre[j].Code) != 0x6000 {
				t.Errorf("create2_deploys[pattern][%d] runtime length: got %d, want 0x6000", i, len(pre[j].Code))
			}
			if embedded := common.BytesToAddress(pre[j].Code[0x2C:0x40]); embedded != addr {
				t.Errorf("create2_deploys[pattern][%d] embedded address mismatch: got %s, want %s",
					i, embedded.Hex(), addr.Hex())
			}
		}
	}

	// Entity 26 — create_preimage_deploys (sender=0x...abcd, start_nonce=2,
	// count=3). Each derived address is crypto.CreateAddress(sender, 2+i);
	// every child carries the literal runtime AND the storage_init slots
	// (0=0x2a, 1=0xcafe, 2=sender), so these are code-AND-storage-bearing.
	{
		sender := common.HexToAddress("0x000000000000000000000000000000000000abcd")
		runtime := common.FromHex("0x615fff565b")
		var wantS0, wantS1, wantS2 common.Hash
		wantS0[31] = 0x2a
		wantS1[30], wantS1[31] = 0xca, 0xfe
		wantS2.SetBytes(sender.Bytes())
		for i := 0; i < 3; i++ {
			addr := crypto.CreateAddress(sender, uint64(2+i))
			j := get("create_preimage_deploys", addr)
			if !bytes.Equal(pre[j].Code, runtime) {
				t.Errorf("create_preimage_deploys[%d] Code: got %x, want %x", i, pre[j].Code, runtime)
			}
			slots := drainStorage(pre[j].Storage)
			var s1 common.Hash
			s1[31] = 1
			var s2 common.Hash
			s2[31] = 2
			if slots[common.Hash{}] != wantS0 || slots[s1] != wantS1 || slots[s2] != wantS2 {
				t.Errorf("create_preimage_deploys[%d] storage_init mismatch: slot0=%s slot1=%s slot2=%s",
					i, slots[common.Hash{}].Hex(), slots[s1].Hex(), slots[s2].Hex())
			}
		}
	}

	// Entity 27 — sequential_eoas (address 0x...5000, count=3). Plain EOAs
	// at 0x5000..0x5002 with NO code and nonce 0. Balance is omitted in
	// the fixture, exercising the 1-wei default (kept off EIP-161 pruning).
	for i := 0; i < 3; i++ {
		var a common.Address
		a[18] = 0x50
		a[19] = byte(i)
		j := get("sequential_eoas", a)
		if pre[j].Code != nil {
			t.Errorf("sequential_eoas[%d] Code: must be nil for plain EOA", i)
		}
		if pre[j].Account.Nonce != 0 {
			t.Errorf("sequential_eoas[%d] nonce: got %d, want 0", i, pre[j].Account.Nonce)
		}
		if pre[j].Account.Balance.IsZero() {
			t.Errorf("sequential_eoas[%d] balance: must default to 1 wei, got 0", i)
		}
	}

	// Entity 28 — sequential_pkey_eoas (start_pkey=0x222…2, count=3). The
	// derived address is secp256k1 PubkeyToAddress of the scalar padded to
	// 32 bytes big-endian; the template enforces nonce=0 and no code.
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
			t.Fatalf("sequential_pkey_eoas[%d] control derive: %v", i, err)
		}
		j := get("sequential_pkey_eoas", crypto.PubkeyToAddress(priv.PublicKey))
		if pre[j].Code != nil {
			t.Errorf("sequential_pkey_eoas[%d] Code: must be nil for plain EOA", i)
		}
		if pre[j].Account.Nonce != 0 {
			t.Errorf("sequential_pkey_eoas[%d] nonce: got %d, want 0", i, pre[j].Account.Nonce)
		}
	}
}

// TestBuildFullMatrixDeterministic pins the strongest contract the
// repricing templates must uphold: same YAML + same seed → byte-
// identical PreAlloc across runs, including every storage slot. The same
// guarantee is what makes the cross-client-genesis-root aggregator's
// invariant work. It runs against the full-matrix fixture so the bloater
// templates' fan-out paths (sequential_eoas, create2_deploys,
// create_preimage_deploys, sequential_pkey_eoas) are all exercised.
//
// Modeled on TestBuildDeterminismEndToEnd. (TestBuildFullMatrix already
// covers cross-client address agreement and warning-free builds for the
// same fixture; this adds the same-opts byte-identity guarantee.)
func TestBuildFullMatrixDeterministic(t *testing.T) {
	s, err := spec.ParseFile("../../examples/full-matrix-spec-feature.yaml")
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
