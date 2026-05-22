package autofill

import (
	mrand "math/rand"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/entitygen"
)

// EOAFlavors carries the per-EOA Bernoulli weights for the two flavor
// dimensions internal/autofill randomizes. The dimensions are independent
// — an EOA can have non-0 balance AND a delegation marker simultaneously.
// Nonce is always non-0; the post-draw bump is unconditional and not a knob.
type EOAFlavors struct {
	HasBalance    float64
	HasDelegation float64
}

// DefaultEOAFlavors returns the mainnet-shaped defaults: 90 % non-0 balance,
// 30 % EIP-7702 delegation marker.
func DefaultEOAFlavors() EOAFlavors {
	return EOAFlavors{
		HasBalance:    0.90,
		HasDelegation: 0.30,
	}
}

// delegationPrefix is the EIP-7702 designation marker. The full 23-byte
// code is delegationPrefix || target20.
var delegationPrefix = []byte{0xef, 0x01, 0x00}

// GenerateEOAFlavored returns an EOA produced by entitygen.GenerateEOA
// post-processed according to flavors.
//
// RNG draw order (matters — all 5 client emission sites consume the same
// sequence for the cross-client root invariant):
//  1. entitygen.GenerateEOA(rng)     — canonical 3 draws.
//  2. rng.Float64()                  — HasBalance Bernoulli.
//  3. rng.Float64()                  — HasDelegation Bernoulli.
//  4. (conditional) rng.Read(target) — only when HasDelegation fires.
//
// The nonce-zero-to-one bump consumes no RNG draws.
func GenerateEOAFlavored(rng *mrand.Rand, flavors EOAFlavors) *entitygen.Account {
	acc := entitygen.GenerateEOA(rng)

	if acc.StateAccount.Nonce == 0 {
		acc.StateAccount.Nonce = 1
	}

	if rng.Float64() < flavors.HasBalance {
		if acc.StateAccount.Balance.IsZero() {
			acc.StateAccount.Balance = uint256.NewInt(1)
		}
	} else {
		acc.StateAccount.Balance = uint256.NewInt(0)
	}

	if rng.Float64() < flavors.HasDelegation {
		var target common.Address
		rng.Read(target[:])
		code := make([]byte, 0, 23)
		code = append(code, delegationPrefix...)
		code = append(code, target[:]...)
		codeHash := crypto.Keccak256Hash(code)
		acc.Code = code
		acc.CodeHash = codeHash
		acc.StateAccount.CodeHash = codeHash.Bytes()
	}

	return acc
}
