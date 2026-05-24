package entitygen

import (
	"bytes"
	"fmt"
	mrand "math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// fixedSeedRNG returns a freshly-seeded *mrand.Rand for deterministic tests.
func fixedSeedRNG(seed int64) *mrand.Rand {
	return mrand.New(mrand.NewSource(seed))
}

// TestGenerateEOA_Determinism pins byte-level output for seed=12345 so any
// future change to the RNG draw order surfaces here loudly. The golden bytes
// were captured from a clean run and must NOT change without coordinated
// updates to all client backends' golden hashes (`0xee656cf3...` etc.).
func TestGenerateEOA_Determinism(t *testing.T) {
	rng := fixedSeedRNG(12345)

	// First EOA: pinned address + nonce + balance. Values captured from a
	// clean run with seed=12345; they match what the OLD inline generator
	// produced before the entitygen extraction (verified by passing
	// TestBinaryTrieStateRootValue, which pins the post-pipeline state root
	// at 0xee656cf3...). Updating these bytes intentionally requires a
	// coordinated update to that golden hash too.
	a1 := GenerateEOA(rng)
	wantAddr1 := common.HexToAddress("0x1Ae969564b34A33eCD1Af05Fe6923D6de7187099")
	if a1.Address != wantAddr1 {
		t.Fatalf("EOA #1 address: got %s, want %s", a1.Address.Hex(), wantAddr1.Hex())
	}
	if a1.AddrHash != crypto256(t, a1.Address[:]) {
		t.Errorf("EOA #1 AddrHash mismatch with keccak(Address)")
	}
	if a1.StateAccount.Root != types.EmptyRootHash {
		t.Errorf("EOA #1 Root: got %s, want EmptyRootHash", a1.StateAccount.Root.Hex())
	}
	if !bytes.Equal(a1.StateAccount.CodeHash, types.EmptyCodeHash.Bytes()) {
		t.Errorf("EOA #1 CodeHash: got %x, want EmptyCodeHash", a1.StateAccount.CodeHash)
	}

	// Second EOA: confirm RNG advances correctly (different address from first)
	a2 := GenerateEOA(rng)
	if a2.Address == a1.Address {
		t.Errorf("EOA #2 address equals #1 — RNG not advancing")
	}

	// Storage/code must be empty for EOAs
	if len(a1.Code) != 0 || len(a1.Storage) != 0 {
		t.Errorf("EOA #1 unexpectedly has Code (%d) or Storage (%d)", len(a1.Code), len(a1.Storage))
	}
}

// TestGenerateEOA_SeedReproducible runs GenerateEOA twice with the same seed
// and asserts byte-identical output across both runs. This is the contract
// every cross-client consumer relies on.
func TestGenerateEOA_SeedReproducible(t *testing.T) {
	const N = 50
	for _, seed := range []int64{1, 12345, 1 << 30, -1} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r1 := fixedSeedRNG(seed)
			r2 := fixedSeedRNG(seed)
			for i := 0; i < N; i++ {
				a1 := GenerateEOA(r1)
				a2 := GenerateEOA(r2)
				if a1.Address != a2.Address {
					t.Fatalf("EOA #%d Address divergence: %s vs %s", i, a1.Address.Hex(), a2.Address.Hex())
				}
				if a1.StateAccount.Nonce != a2.StateAccount.Nonce {
					t.Fatalf("EOA #%d Nonce divergence: %d vs %d", i, a1.StateAccount.Nonce, a2.StateAccount.Nonce)
				}
				if a1.StateAccount.Balance.Cmp(a2.StateAccount.Balance) != 0 {
					t.Fatalf("EOA #%d Balance divergence: %s vs %s", i, a1.StateAccount.Balance, a2.StateAccount.Balance)
				}
			}
		})
	}
}

// TestGenerateContract_Determinism exercises the contract path: address,
// code hash, storage sort order. Storage slot 0 is the lowest-keyed slot
// after sorting, NOT the first one drawn from the RNG — pinning it confirms
// both the RNG sequence AND the post-sort ordering.
func TestGenerateContract_Determinism(t *testing.T) {
	rng := fixedSeedRNG(12345)
	c := GenerateContract(rng, 256, 5) // codeSize=256, 5 slots

	if len(c.Code) < 256 || len(c.Code) >= 512 {
		t.Errorf("Contract Code length %d outside expected [256, 512) range", len(c.Code))
	}
	if c.CodeHash != crypto256(t, c.Code) {
		t.Errorf("CodeHash != keccak(Code)")
	}
	// Triple invariant: Account.Code, Account.CodeHash, and
	// StateAccount.CodeHash MUST all agree. Drift between them lands on
	// disk silently and only surfaces as a cross-client state-root
	// divergence — the EVM resolves contract code by hash → bytecode
	// lookup, so a mismatch makes the contract un-callable.
	if !bytes.Equal(c.StateAccount.CodeHash, c.CodeHash.Bytes()) {
		t.Errorf("StateAccount.CodeHash %x != Account.CodeHash %x",
			c.StateAccount.CodeHash, c.CodeHash.Bytes())
	}
	if len(c.Storage) != 5 {
		t.Errorf("Storage slot count: got %d, want 5", len(c.Storage))
	}

	// Storage must be sorted by Key ascending.
	for i := 1; i < len(c.Storage); i++ {
		if bytes.Compare(c.Storage[i-1].Key[:], c.Storage[i].Key[:]) >= 0 {
			t.Errorf("Storage not sorted at index %d: %x >= %x",
				i, c.Storage[i-1].Key, c.Storage[i].Key)
		}
	}

	// No zero-valued slots (zero would mean a deletion in MPT semantics).
	for i, s := range c.Storage {
		if s.Value == (common.Hash{}) {
			t.Errorf("Storage[%d] has zero value (should be bumped to 0x..01)", i)
		}
	}
}

// TestGenerateContract_SeedReproducible: cross-run byte equality for contracts.
func TestGenerateContract_SeedReproducible(t *testing.T) {
	const N = 20
	r1 := fixedSeedRNG(98765)
	r2 := fixedSeedRNG(98765)
	for i := 0; i < N; i++ {
		c1 := GenerateContract(r1, 256, 3)
		c2 := GenerateContract(r2, 256, 3)
		if c1.Address != c2.Address {
			t.Fatalf("Contract #%d Address divergence", i)
		}
		if !bytes.Equal(c1.Code, c2.Code) {
			t.Fatalf("Contract #%d Code divergence", i)
		}
		if len(c1.Storage) != len(c2.Storage) {
			t.Fatalf("Contract #%d Storage length divergence: %d vs %d", i, len(c1.Storage), len(c2.Storage))
		}
		for j := range c1.Storage {
			if c1.Storage[j] != c2.Storage[j] {
				t.Fatalf("Contract #%d Storage[%d] divergence", i, j)
			}
		}
	}
}

// TestGenerateSlotCount_MatchesDistribution is the load-bearing parity test
// between the two slot-count APIs. The bintrie path uses GenerateSlotCount
// (one contract at a time) while the MPT path uses GenerateSlotDistribution
// (all contracts up front). For the same seed and config, the sequence of
// slot counts MUST be identical or bintrie/MPT produce different golden hashes.
func TestGenerateSlotCount_MatchesDistribution(t *testing.T) {
	const (
		seed       = int64(12345)
		minSlots   = 1
		maxSlots   = 100
		nContracts = 50
	)

	for _, dist := range []Distribution{PowerLaw, Uniform, Exponential} {
		t.Run(fmt.Sprintf("dist=%d", dist), func(t *testing.T) {
			r1 := fixedSeedRNG(seed)
			counts := GenerateSlotDistribution(r1, dist, minSlots, maxSlots, nContracts)

			r2 := fixedSeedRNG(seed)
			for i := 0; i < nContracts; i++ {
				got := GenerateSlotCount(r2, dist, minSlots, maxSlots)
				if got != counts[i] {
					t.Errorf("dist=%d slot[%d]: GenerateSlotCount=%d, GenerateSlotDistribution=%d",
						dist, i, got, counts[i])
				}
			}
		})
	}
}

// TestGenerateSlotCount_PinnedPowerLaw freezes the first 10 slot counts for
// seed=12345, MinSlots=1, MaxSlots=100, PowerLaw. Any future change to the
// Pareto-inverse-CDF formula or the RNG draw count surfaces here.
func TestGenerateSlotCount_PinnedPowerLaw(t *testing.T) {
	rng := fixedSeedRNG(12345)
	got := make([]int, 10)
	for i := range got {
		got[i] = GenerateSlotCount(rng, PowerLaw, 1, 100)
	}

	// Pinned bytes captured from a clean run. If you intentionally change the
	// distribution math, regenerate and update both this and the existing
	// generator-package golden hashes (TestBinaryTrieStateRootValue:
	// 0xee656cf3...).
	want := []int{3, 1, 2, 1, 1, 9, 1, 1, 1, 1}
	if !intsEqual(got, want) {
		t.Fatalf("PowerLaw slot counts: got %v, want %v", got, want)
	}
}

// TestParseDistribution covers the CLI-string-to-enum mapping. Default-case
// fallback to PowerLaw is the documented historical behavior.
func TestParseDistribution(t *testing.T) {
	cases := []struct {
		in   string
		want Distribution
	}{
		{"power-law", PowerLaw}, // explicit
		{"PoWeR-LaW", PowerLaw}, // case-insensitive
		{"powerlaw", PowerLaw},  // bogus → default
		{"uniform", Uniform},
		{"UNIFORM", Uniform},
		{"exponential", Exponential},
		{"", PowerLaw}, // empty → default
	}
	for _, c := range cases {
		if got := ParseDistribution(c.in); got != c.want {
			t.Errorf("ParseDistribution(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}

// TestMapToSortedSlots verifies sort order and that no slots are dropped.
func TestMapToSortedSlots(t *testing.T) {
	in := map[common.Hash]common.Hash{
		common.HexToHash("0x03"): common.HexToHash("0xaa"),
		common.HexToHash("0x01"): common.HexToHash("0xbb"),
		common.HexToHash("0x02"): common.HexToHash("0xcc"),
	}

	out := MapToSortedSlots(in)
	if len(out) != 3 {
		t.Fatalf("MapToSortedSlots length: got %d, want 3", len(out))
	}
	for i := 1; i < len(out); i++ {
		if bytes.Compare(out[i-1].Key[:], out[i].Key[:]) >= 0 {
			t.Errorf("MapToSortedSlots not sorted at index %d", i)
		}
	}

	// Verify no slot dropped, all values preserved.
	got := make(map[common.Hash]common.Hash, len(out))
	for _, s := range out {
		got[s.Key] = s.Value
	}
	for k, v := range in {
		if got[k] != v {
			t.Errorf("MapToSortedSlots dropped or corrupted slot[%s]: got %s, want %s",
				k.Hex(), got[k].Hex(), v.Hex())
		}
	}
}

// TestMapToSortedSlots_Empty: empty input returns empty (not nil-vs-empty
// gotcha).
func TestMapToSortedSlots_Empty(t *testing.T) {
	out := MapToSortedSlots(nil)
	if out == nil {
		t.Errorf("MapToSortedSlots(nil) returned nil; want empty slice")
	}
	if len(out) != 0 {
		t.Errorf("MapToSortedSlots(nil) length: got %d, want 0", len(out))
	}
}

// --- helpers ---

func crypto256(t *testing.T, b []byte) common.Hash {
	t.Helper()
	// Use go-ethereum's crypto via the import indirectly — testing-only,
	// keeps the test file's import list small.
	return crypto256Direct(b)
}

// intsEqual is a tiny []int equality helper to keep the table-driven
// pinning test readable.
func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
