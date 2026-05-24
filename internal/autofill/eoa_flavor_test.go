package autofill

import (
	"bytes"
	mrand "math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nerolation/state-actor/internal/entitygen"
)

func TestGenerateEOAFlavored_NonceAlwaysNonZero(t *testing.T) {
	const N = 5000
	rng := mrand.New(mrand.NewSource(42))
	flavors := DefaultEOAFlavors()
	for i := range N {
		acc := GenerateEOAFlavored(rng, flavors)
		if acc.StateAccount.Nonce == 0 {
			t.Fatalf("EOA[%d]: nonce was zero; flavor contract violated", i)
		}
	}
}

func TestGenerateEOAFlavored_BernoulliRates(t *testing.T) {
	const N = 10000
	rng := mrand.New(mrand.NewSource(42))
	flavors := EOAFlavors{HasBalance: 0.90, HasDelegation: 0.30}

	var withBalance, withDelegation int
	for range N {
		acc := GenerateEOAFlavored(rng, flavors)
		if !acc.StateAccount.Balance.IsZero() {
			withBalance++
		}
		if len(acc.Code) > 0 {
			withDelegation++
		}
	}

	// Allow ±3σ around the binomial expectation.
	// σ_balance    ≈ sqrt(10000 × 0.9 × 0.1) = 30  → 3σ = 90
	// σ_delegation ≈ sqrt(10000 × 0.3 × 0.7) ≈ 46  → 3σ ≈ 138
	if withBalance < 9000-90 || withBalance > 9000+90 {
		t.Errorf("non-zero balance count: got %d, want ~9000 ±90", withBalance)
	}
	if withDelegation < 3000-138 || withDelegation > 3000+138 {
		t.Errorf("delegation count: got %d, want ~3000 ±138", withDelegation)
	}
}

func TestGenerateEOAFlavored_DelegationMarkerFormat(t *testing.T) {
	rng := mrand.New(mrand.NewSource(42))
	flavors := EOAFlavors{HasBalance: 0, HasDelegation: 1.0}
	for i := range 100 {
		acc := GenerateEOAFlavored(rng, flavors)
		if len(acc.Code) != 23 {
			t.Errorf("EOA[%d]: code length got %d, want 23 (3-byte prefix + 20-byte addr)", i, len(acc.Code))
			continue
		}
		if acc.Code[0] != 0xef || acc.Code[1] != 0x01 || acc.Code[2] != 0x00 {
			t.Errorf("EOA[%d]: prefix got %x, want ef0100", i, acc.Code[:3])
		}
	}
}

// TestGenerateEOAFlavored_DelegationCodeHashMatchesCode pins the triple
// invariant for EIP-7702 delegating EOAs: acc.Code, acc.CodeHash, and
// acc.StateAccount.CodeHash MUST all agree. A future refactor that
// updates one without the others would land on disk silently and only
// surface as a cross-client state-root divergence (much further from
// the root cause than necessary). The EVM resolves delegation by hash
// → bytecode lookup, so a drift makes the delegated contract un-callable.
func TestGenerateEOAFlavored_DelegationCodeHashMatchesCode(t *testing.T) {
	rng := mrand.New(mrand.NewSource(42))
	flavors := EOAFlavors{HasBalance: 0, HasDelegation: 1.0}
	for i := range 100 {
		acc := GenerateEOAFlavored(rng, flavors)
		if len(acc.Code) != 23 {
			t.Fatalf("EOA[%d]: code length %d, want 23", i, len(acc.Code))
		}
		wantHash := crypto.Keccak256Hash(acc.Code)
		if acc.CodeHash != wantHash {
			t.Errorf("EOA[%d]: Account.CodeHash %s != keccak(Code) %s",
				i, acc.CodeHash.Hex(), wantHash.Hex())
		}
		if !bytes.Equal(acc.StateAccount.CodeHash, wantHash.Bytes()) {
			t.Errorf("EOA[%d]: StateAccount.CodeHash %x != keccak(Code) %x",
				i, acc.StateAccount.CodeHash, wantHash.Bytes())
		}
	}
}

func TestGenerateEOAFlavored_CanonicalEOAFirst(t *testing.T) {
	// GenerateEOAFlavored MUST call entitygen.GenerateEOA first; the returned
	// Account's Address must match what plain GenerateEOA produces from the
	// same seeded RNG state. Guards against any future refactor that moves
	// flavor draws before the canonical sequence.
	rngPlain := mrand.New(mrand.NewSource(42))
	rngFlav := mrand.New(mrand.NewSource(42))

	plain := entitygen.GenerateEOA(rngPlain)
	flav := GenerateEOAFlavored(rngFlav, DefaultEOAFlavors())

	if plain.Address != flav.Address {
		t.Errorf("address divergence: plain=%s flav=%s", plain.Address.Hex(), flav.Address.Hex())
	}
}
