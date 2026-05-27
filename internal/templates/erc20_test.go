package templates

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func TestERC20ValidateParameters(t *testing.T) {
	tmpl := erc20Template{}
	cases := []struct {
		name   string
		params map[string]any
		valid  bool
	}{
		{
			name:   "complete-bare",
			params: map[string]any{"symbol": "USDC", "name": "USD Coin", "decimals": 18},
			valid:  true,
		},
		{
			name:   "missing-symbol",
			params: map[string]any{"name": "x", "decimals": 18},
		},
		{
			name:   "missing-name",
			params: map[string]any{"symbol": "x", "decimals": 18},
		},
		{
			name:   "missing-decimals",
			params: map[string]any{"symbol": "x", "name": "x"},
		},
		{
			name:   "decimals-not-18",
			params: map[string]any{"symbol": "x", "name": "x", "decimals": 6},
		},
		{
			name:   "decimals-wrong-type",
			params: map[string]any{"symbol": "x", "name": "x", "decimals": "18"},
		},
		{
			name:   "symbol-too-long",
			params: map[string]any{"symbol": "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567", "name": "x", "decimals": 18},
		},
		{
			name:   "name-too-long",
			params: map[string]any{"symbol": "x", "name": "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567", "decimals": 18},
		},
		{
			name:   "unknown-key",
			params: map[string]any{"symbol": "x", "name": "x", "decimals": 18, "supply": 1_000_000},
		},
		{
			name:   "holders-removed",
			params: map[string]any{"symbol": "x", "name": "x", "decimals": 18, "holders": 1000},
		},
		{
			name: "total_owners-only-ok",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"total_owners": 1000,
			},
			valid: true,
		},
		{
			name: "owners-explicit-ok",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0x1111111111111111111111111111111111111111", "balance": "100"},
				},
			},
			valid: true,
		},
		{
			name: "owners-allowances-and-totals-ok",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0x1111111111111111111111111111111111111111", "balance": "100"},
				},
				"total_owners": 5,
				"allowances": []any{
					map[string]any{
						"owner":     "0x1111111111111111111111111111111111111111",
						"spender":   "0x2222222222222222222222222222222222222222",
						"allowance": "50",
					},
				},
				"total_allowances": 3,
			},
			valid: true,
		},
		{
			name: "owners-duplicate-address",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0x1111111111111111111111111111111111111111", "balance": "100"},
					map[string]any{"address": "0x1111111111111111111111111111111111111111", "balance": "200"},
				},
			},
		},
		{
			name: "allowances-duplicate-pair",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"allowances": []any{
					map[string]any{"owner": "0x1111111111111111111111111111111111111111", "spender": "0x2222222222222222222222222222222222222222", "allowance": "10"},
					map[string]any{"owner": "0x1111111111111111111111111111111111111111", "spender": "0x2222222222222222222222222222222222222222", "allowance": "20"},
				},
			},
		},
		{
			name: "total_owners-less-than-explicit",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0x1111111111111111111111111111111111111111", "balance": "100"},
					map[string]any{"address": "0x2222222222222222222222222222222222222222", "balance": "200"},
				},
				"total_owners": 1,
			},
		},
		{
			name: "owner-missing-balance",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0x1111111111111111111111111111111111111111"},
				},
			},
		},
		{
			name: "owner-bad-address",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0xZZZZ", "balance": "100"},
				},
			},
		},
		{
			name: "owner-unquoted-balance",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"owners": []any{
					map[string]any{"address": "0x1111111111111111111111111111111111111111", "balance": 100},
				},
			},
		},
		{
			name: "allowance-missing-owner",
			params: map[string]any{
				"symbol": "x", "name": "x", "decimals": 18,
				"allowances": []any{
					map[string]any{"spender": "0x2222222222222222222222222222222222222222", "allowance": "10"},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tmpl.ValidateParameters(tc.params)
			if tc.valid && err != nil {
				t.Errorf("expected pass, got %v", err)
			}
			if !tc.valid && err == nil {
				t.Errorf("expected fail, got nil")
			}
		})
	}
}

func TestERC20StorageLayout(t *testing.T) {
	addr := common.HexToAddress("0x0000000000000000000000000000000000000006")
	const seed = int64(1)
	const totalOwners = 3
	ent := spec.Entity{
		Kind:     spec.KindContract,
		Template: "erc20",
		Parameters: map[string]any{
			"symbol": "TEST", "name": "TestToken", "decimals": 18,
			"total_owners": totalOwners,
		},
	}
	ctx := Context{
		Seed: seed, ClientName: "geth",
		Sizer:           fixedSizer{bytesPerSlot: 64},
		ResolvedAddress: addr,
	}

	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	storage := collectMap(out[0].Storage)

	// _name slot — short-string layout for "TestToken" (9 bytes → 0x12 at byte 31).
	nameSlot := storage[uint64SlotKey(erc20SlotName)]
	if nameSlot[31] != 18 { // 9 chars × 2 = 18
		t.Errorf("name slot length byte = 0x%02x, want 0x12", nameSlot[31])
	}
	if !bytes.HasPrefix(nameSlot[:], []byte("TestToken")) {
		t.Errorf("name slot prefix wrong: %x", nameSlot)
	}

	// _symbol slot — "TEST" (4 bytes → 0x08).
	symSlot := storage[uint64SlotKey(erc20SlotSymbol)]
	if symSlot[31] != 8 {
		t.Errorf("symbol slot length byte = 0x%02x, want 0x08", symSlot[31])
	}

	// _totalSupply auto-summed from the synthesized random balances —
	// re-derive the same values via DeterministicRandomBalance and
	// compare slot bytes.
	wantTotal := new(uint256.Int)
	for i := 0; i < totalOwners; i++ {
		v := DeterministicRandomBalance(seed, addr, i)
		wantTotal.Add(wantTotal, new(uint256.Int).SetBytes(v[:]))
	}
	gotTotal := storage[uint64SlotKey(erc20SlotTotalSupply)]
	if gotTotal != wantTotal.Bytes32() {
		t.Errorf("totalSupply mismatch:\n got  %x\n want %x", gotTotal, wantTotal.Bytes32())
	}

	// Storage shape: 3 fixed slots (name, symbol, totalSupply) + 3
	// synthesized _balances slots = 6 entries total.
	if len(storage) != 6 {
		t.Errorf("storage entry count = %d, want 6", len(storage))
	}
}

func TestERC20BalancesSlotComputationMatchesSolidity(t *testing.T) {
	// Solidity rule: slot(mapping[k]) = keccak256(abi.encode(k, mappingSlot))
	// where k is the address (left-padded to 32 bytes) and mappingSlot is
	// the slot index (left-padded to 32 bytes).
	//
	// Verify our erc20BalancesIter produces a key for the first synthesized
	// holder that matches what Solidity would compute for that holder.
	addr := common.HexToAddress("0x0000000000000000000000000000000000000007")
	const seed = int64(99)
	const count = 1
	pairs := collectPairs(erc20BalancesIter(seed, addr, count))
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(pairs))
	}

	// Reconstruct the holder address the iterator would have used.
	var preimage [8 + common.AddressLength + 8]byte
	preimage[0] = 0
	preimage[1] = 0
	preimage[2] = 0
	preimage[3] = 0
	preimage[4] = 0
	preimage[5] = 0
	preimage[6] = 0
	preimage[7] = byte(seed) // seed=99 fits in one byte
	copy(preimage[8:8+common.AddressLength], addr[:])
	// last 8 bytes are zero for i=0
	holderHashFull := crypto.Keccak256(preimage[:])

	// Compute the expected mapping slot key the Solidity way.
	var expected [64]byte
	copy(expected[12:32], holderHashFull[12:]) // address left-padded
	// slot index 0 already zero-filled at 32..63
	expectedKey := crypto.Keccak256Hash(expected[:])

	if pairs[0].K != expectedKey {
		t.Errorf("balance slot key:\n got  %s\n want %s", pairs[0].K.Hex(), expectedKey.Hex())
	}
}

// TestERC20BalancesSlotComputationManyHolders extends the single-holder
// Solidity-equivalence check to multiple holders so a buffer-reuse bug
// or off-by-one in the iterator's index mutation (erc20.go:185) would
// surface. Each holder's _balances[h] slot must equal Solidity's
// keccak256(pad32(h) || pad32(0)) rule.
func TestERC20BalancesSlotComputationManyHolders(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000beef")
	const seed = int64(99)
	const count = 25

	pairs := collectPairs(erc20BalancesIter(seed, addr, count))
	if len(pairs) != count {
		t.Fatalf("got %d pairs, want %d", len(pairs), count)
	}

	// Per-iteration: re-derive the expected slot key from scratch using
	// the iterator's input recipe.
	var preimage [8 + common.AddressLength + 8]byte
	preimage[7] = byte(seed) // seed=99 fits in one byte
	copy(preimage[8:8+common.AddressLength], addr[:])

	var mapKey [64]byte // leftPad32(holder) || leftPad32(slot=0)
	for i := 0; i < count; i++ {
		// Index in the trailing 8 bytes of preimage.
		for j := 0; j < 7; j++ {
			preimage[8+common.AddressLength+j] = 0
		}
		preimage[8+common.AddressLength+7] = byte(i)
		holderHash := crypto.Keccak256(preimage[:])

		// Build mapKey: clear holder bytes then copy holder address (right-12).
		for j := 0; j < 32; j++ {
			mapKey[j] = 0
		}
		copy(mapKey[12:32], holderHash[12:])
		expected := crypto.Keccak256Hash(mapKey[:])

		if pairs[i].K != expected {
			t.Errorf("holder %d slot mismatch:\n got  %s\n want %s",
				i, pairs[i].K.Hex(), expected.Hex())
		}
	}
}

// TestERC20NonceHonorsUserValue pins the contract about nonce:
// user-supplied nonce wins, but nonce=0 (the unset YAML default) floors
// to nonce=1 per EIP-161.
func TestERC20NonceHonorsUserValue(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	ctx := Context{Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: addr}

	cases := []struct {
		name     string
		setNonce uint64
		want     uint64
	}{
		{"unset (default 1)", 0, 1},
		{"explicit-1", 1, 1},
		{"explicit-42", 42, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ent := spec.Entity{
				Kind:     spec.KindContract,
				Template: "erc20",
				Nonce:    tc.setNonce,
				Parameters: map[string]any{
					"symbol": "X", "name": "X", "decimals": 18,
				},
			}
			out, err := erc20Template{}.Expand(ctx, ent)
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if got := out[0].Account.Nonce; got != tc.want {
				t.Errorf("nonce: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestERC20OwnersExplicit verifies each explicit owner's balance slot
// lands at the correct Solidity `keccak(pad32(addr) || pad32(0))` key
// with the expected value.
func TestERC20OwnersExplicit(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000aa1")
	alice := common.HexToAddress("0x1111111111111111111111111111111111111111")
	bob := common.HexToAddress("0x2222222222222222222222222222222222222222")
	ent := spec.Entity{
		Kind:     spec.KindContract,
		Template: "erc20",
		Parameters: map[string]any{
			"symbol": "AAA", "name": "ExplicitOwnersToken", "decimals": 18,
			"owners": []any{
				map[string]any{"address": alice.Hex(), "balance": "1000"},
				map[string]any{"address": bob.Hex(), "balance": "500"},
			},
		},
	}
	ctx := Context{Seed: 7, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)

	wantAlice := uint256.NewInt(1000).Bytes32()
	gotAlice := storage[balanceSlotKey(alice)]
	if gotAlice != wantAlice {
		t.Errorf("alice _balances slot:\n got  %x\n want %x", gotAlice, wantAlice)
	}
	wantBob := uint256.NewInt(500).Bytes32()
	gotBob := storage[balanceSlotKey(bob)]
	if gotBob != wantBob {
		t.Errorf("bob _balances slot:\n got  %x\n want %x", gotBob, wantBob)
	}
}

// TestERC20AllowancesExplicit verifies each explicit allowance lands at
// the correct nested-mapping slot:
//
//	inner = keccak(pad32(owner)   || pad32(1))
//	slot  = keccak(pad32(spender) || inner)
func TestERC20AllowancesExplicit(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000aa1")
	alice := common.HexToAddress("0x1111111111111111111111111111111111111111")
	spender := common.HexToAddress("0x3333333333333333333333333333333333333333")
	ent := spec.Entity{
		Kind:     spec.KindContract,
		Template: "erc20",
		Parameters: map[string]any{
			"symbol": "AAA", "name": "T", "decimals": 18,
			"allowances": []any{
				map[string]any{"owner": alice.Hex(), "spender": spender.Hex(), "allowance": "100"},
			},
		},
	}
	ctx := Context{Seed: 7, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)
	want := uint256.NewInt(100).Bytes32()
	got := storage[allowanceSlotKey(alice, spender)]
	if got != want {
		t.Errorf("allowance slot:\n got  %x\n want %x", got, want)
	}
}

// TestERC20TotalOwnersGapFill verifies that explicit owners + random
// fill combine to exactly total_owners distinct _balances slots.
func TestERC20TotalOwnersGapFill(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000aa3")
	a := common.HexToAddress("0x1111111111111111111111111111111111111111")
	b := common.HexToAddress("0x2222222222222222222222222222222222222222")
	c := common.HexToAddress("0x4444444444444444444444444444444444444444")
	ent := spec.Entity{
		Kind:     spec.KindContract,
		Template: "erc20",
		Parameters: map[string]any{
			"symbol": "CCC", "name": "Mixed", "decimals": 18,
			"owners": []any{
				map[string]any{"address": a.Hex(), "balance": "1000"},
				map[string]any{"address": b.Hex(), "balance": "500"},
				map[string]any{"address": c.Hex(), "balance": "250"},
			},
			"total_owners": 10,
		},
	}
	ctx := Context{Seed: 13, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)

	// Expect 3 fixed slots (name, symbol, totalSupply) + 10 _balances slots = 13.
	if len(storage) != 13 {
		t.Errorf("storage count = %d, want 13", len(storage))
	}
	// All three explicit owners must have their planted balances.
	for _, want := range []struct {
		addr common.Address
		bal  uint64
	}{{a, 1000}, {b, 500}, {c, 250}} {
		got := storage[balanceSlotKey(want.addr)]
		wantHash := uint256.NewInt(want.bal).Bytes32()
		if got != wantHash {
			t.Errorf("explicit owner %s: got %x, want %x", want.addr.Hex(), got, wantHash)
		}
	}
}

// TestERC20TotalSupplyAutoSummed verifies _totalSupply equals the sum of
// every planted balance (explicit + random).
func TestERC20TotalSupplyAutoSummed(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000aa3")
	a := common.HexToAddress("0x1111111111111111111111111111111111111111")
	const seed = int64(17)
	const totalOwners = 5
	ent := spec.Entity{
		Kind:     spec.KindContract,
		Template: "erc20",
		Parameters: map[string]any{
			"symbol": "X", "name": "X", "decimals": 18,
			"owners": []any{
				map[string]any{"address": a.Hex(), "balance": "1000"},
			},
			"total_owners": totalOwners,
		},
	}
	ctx := Context{Seed: seed, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)

	want := uint256.NewInt(1000)
	for i := 0; i < totalOwners-1; i++ {
		v := DeterministicRandomBalance(seed, tokenAddr, i)
		want.Add(want, new(uint256.Int).SetBytes(v[:]))
	}
	got := storage[uint64SlotKey(erc20SlotTotalSupply)]
	if got != want.Bytes32() {
		t.Errorf("totalSupply:\n got  %x\n want %x", got, want.Bytes32())
	}
}

// TestERC20SynthesizedAllowanceDeterminism pins re-iteration of
// erc20RandomAllowancesIter: same (seed, tokenAddr, count) → byte-identical
// slot stream. Cross-client invariance depends on this.
func TestERC20SynthesizedAllowanceDeterminism(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000aa3")
	pairs1 := collectPairs(erc20RandomAllowancesIter(42, tokenAddr, 8))
	pairs2 := collectPairs(erc20RandomAllowancesIter(42, tokenAddr, 8))
	if len(pairs1) != len(pairs2) || len(pairs1) != 8 {
		t.Fatalf("got len=%d/%d, want 8", len(pairs1), len(pairs2))
	}
	for i := range pairs1 {
		if pairs1[i] != pairs2[i] {
			t.Errorf("pair %d diverged:\n run1 K=%x V=%x\n run2 K=%x V=%x",
				i, pairs1[i].K, pairs1[i].V, pairs2[i].K, pairs2[i].V)
		}
	}
}

// TestERC20ApproximateSizeBytesDrivesTotalOwners pins the fallback-sizing
// behavior: when neither total_owners nor total_allowances is set,
// approximate_size_bytes derives totalOwners via the Sizer, matching the
// universal sizing semantics raw/eoa already implement.
func TestERC20ApproximateSizeBytesDrivesTotalOwners(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000ab1")
	// With Sizer.bytesPerSlot=64: 6400 bytes → 100 slots → 100-3 fixed
	// = 97 random owners + 3 fixed metadata slots = 100 entries total.
	ent := spec.Entity{
		Kind:                 spec.KindContract,
		Template:             "erc20",
		ApproximateSizeBytes: 6400,
		Parameters: map[string]any{
			"symbol": "BIG", "name": "BigToken", "decimals": 18,
		},
	}
	ctx := Context{Seed: 1, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)
	if len(storage) != 100 {
		t.Errorf("storage count: got %d, want 100", len(storage))
	}
}

// TestERC20ApproximateSizeBytesEquivalentToTotalOwners pins the
// equivalence: setting only approximate_size_bytes must produce the same
// storage stream as setting an explicit total_owners equal to the
// derived value. This is the cross-template invariance gate
// (truncateForTargetSize's projection math already assumed this).
func TestERC20ApproximateSizeBytesEquivalentToTotalOwners(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000ab2")
	const bytesPerSlot uint64 = 64
	const targetBytes uint64 = 6400
	// Derived count: (6400 / 64) - 3 fixed = 100 - 3 = 97.
	const expectedTotalOwners = 97

	ctx := Context{Seed: 7, Sizer: fixedSizer{bytesPerSlot: bytesPerSlot}, ResolvedAddress: tokenAddr}

	approxEnt := spec.Entity{
		Kind: spec.KindContract, Template: "erc20",
		ApproximateSizeBytes: targetBytes,
		Parameters: map[string]any{
			"symbol": "X", "name": "X", "decimals": 18,
		},
	}
	explicitEnt := spec.Entity{
		Kind: spec.KindContract, Template: "erc20",
		Parameters: map[string]any{
			"symbol": "X", "name": "X", "decimals": 18,
			"total_owners": expectedTotalOwners,
		},
	}

	approxOut, err := erc20Template{}.Expand(ctx, approxEnt)
	if err != nil {
		t.Fatalf("approx Expand: %v", err)
	}
	explicitOut, err := erc20Template{}.Expand(ctx, explicitEnt)
	if err != nil {
		t.Fatalf("explicit Expand: %v", err)
	}

	approxStorage := collectMap(approxOut[0].Storage)
	explicitStorage := collectMap(explicitOut[0].Storage)

	if len(approxStorage) != len(explicitStorage) {
		t.Fatalf("storage count diverged: approx=%d explicit=%d",
			len(approxStorage), len(explicitStorage))
	}
	for k, v := range approxStorage {
		if got, ok := explicitStorage[k]; !ok || got != v {
			t.Errorf("slot %s diverged: approx=%x explicit=%x (present=%v)",
				k.Hex(), v, got, ok)
		}
	}
}

// TestERC20ApproximateSizeBytesExplicitWins verifies precedence:
// total_owners (or total_allowances) explicitly set always wins over
// approximate_size_bytes — matching the documented "explicit > implicit"
// rule.
func TestERC20ApproximateSizeBytesExplicitWins(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000ab3")
	ent := spec.Entity{
		Kind: spec.KindContract, Template: "erc20",
		ApproximateSizeBytes: 1_000_000, // ~15,625 slots at 64 B/slot
		Parameters: map[string]any{
			"symbol": "X", "name": "X", "decimals": 18,
			"total_owners": 5, // explicit — wins
		},
	}
	ctx := Context{Seed: 1, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)
	// 5 explicit-driven random owners + 3 fixed (name, symbol, totalSupply).
	if len(storage) != 8 {
		t.Errorf("storage count: got %d, want 8 (5 random + 3 fixed)", len(storage))
	}
}

// TestERC20ApproximateSizeBytesNeverShrinksExplicitOwners verifies the
// floor: explicit owners always land, even when the derived totalOwners
// from approximate_size_bytes is smaller than len(owners).
func TestERC20ApproximateSizeBytesNeverShrinksExplicitOwners(t *testing.T) {
	tokenAddr := common.HexToAddress("0x0000000000000000000000000000000000000ab4")
	a := common.HexToAddress("0x1111111111111111111111111111111111111111")
	b := common.HexToAddress("0x2222222222222222222222222222222222222222")
	c := common.HexToAddress("0x3333333333333333333333333333333333333333")
	d := common.HexToAddress("0x4444444444444444444444444444444444444444")
	// approximate_size_bytes = 256 B → 4 slots → 4-3 fixed = 1 derived owner.
	// But 4 explicit owners are set; the floor must keep all 4.
	ent := spec.Entity{
		Kind: spec.KindContract, Template: "erc20",
		ApproximateSizeBytes: 256,
		Parameters: map[string]any{
			"symbol": "X", "name": "X", "decimals": 18,
			"owners": []any{
				map[string]any{"address": a.Hex(), "balance": "100"},
				map[string]any{"address": b.Hex(), "balance": "200"},
				map[string]any{"address": c.Hex(), "balance": "300"},
				map[string]any{"address": d.Hex(), "balance": "400"},
			},
		},
	}
	ctx := Context{Seed: 1, Sizer: fixedSizer{bytesPerSlot: 64}, ResolvedAddress: tokenAddr}
	out, err := erc20Template{}.Expand(ctx, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	storage := collectMap(out[0].Storage)
	for _, addr := range []common.Address{a, b, c, d} {
		if _, ok := storage[balanceSlotKey(addr)]; !ok {
			t.Errorf("explicit owner %s dropped", addr.Hex())
		}
	}
}

func TestERC20RuntimeBytecodePinned(t *testing.T) {
	// Guards against unintentional changes to the vendored OZ v5.6.1
	// ERC20 deployed runtime bytecode (internal/templates/erc20_oz_v5.hex).
	// If scripts/regen-erc20-bytecode.sh is re-run against a different OZ
	// tag or solc version, this hash must be updated alongside the .hex
	// file.
	want := "fe5269d44e5721ea4127b444fd44577c7bfb0b0ebb6ea07bf076fd7cf4cb0b88"
	got := hex.EncodeToString(crypto.Keccak256(ERC20RuntimeBytecode))
	if got != want {
		t.Errorf("ERC20RuntimeBytecode keccak256 changed:\n got  %s\n want %s\n(intentional bytecode regen? update this test alongside the .hex file)", got, want)
	}
}
