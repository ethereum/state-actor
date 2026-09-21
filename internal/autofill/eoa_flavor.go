package autofill

import (
	"encoding/binary"
	mrand "math/rand"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/entitygen"
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
// 2 % EIP-7702 delegation. 2 % is a deliberate floor, not a measured rate:
// 30 % made designators 94.6 % of Besu's code CF (mainnet 4.2 %).
//
// Designators land in CODE_STORAGE keyed by their own code hash (confirmed
// against a live mainnet read: 23 B records are ~0.13 % of cf07 by count),
// same as any contract's code. They fall below every ≥1 KiB compressibility
// gate in internal/autofill/code_pool_compress_test.go, so the code-pool
// compression fix (see code_pool.go) neither touches nor is affected by
// this population; the 2 % floor is out of scope for that fix.
func DefaultEOAFlavors() EOAFlavors {
	return EOAFlavors{
		HasBalance:    0.90,
		HasDelegation: 0.02,
	}
}

// delegationPrefix is the EIP-7702 designation marker. The full 23-byte
// code is delegationPrefix || target20.
var delegationPrefix = []byte{0xef, 0x01, 0x00}

// DelegationTargetPoolSize caps distinct designators: real 7702 authorities
// cluster on a few wallet implementations.
const DelegationTargetPoolSize = 256

// delegationTargets is fixed (not --seed-derived) so every client draws
// identical targets.
var delegationTargets = func() (t [DelegationTargetPoolSize]common.Address) {
	for i := range t {
		copy(t[i][:], crypto.Keccak256(binary.BigEndian.AppendUint64([]byte("state-actor/eip7702-target/v1"), uint64(i)))[12:])
	}
	return t
}()

// GenerateEOAFlavored returns an EOA produced by entitygen.GenerateEOA
// post-processed according to flavors.
//
// RNG draw order (matters — all client emission sites consume the same
// sequence for the cross-client root invariant):
//  1. entitygen.GenerateEOA(rng)  — canonical 3 draws.
//  2. rng.Float64()               — HasBalance Bernoulli.
//  3. rng.Float64()               — HasDelegation Bernoulli.
//  4. (conditional) rng.Intn(DelegationTargetPoolSize) — pool index, only when 3 fires.
//
// The nonce-zero-to-one bump consumes no RNG draws.
func GenerateEOAFlavored(rng *mrand.Rand, flavors EOAFlavors) *entitygen.Account {
	return generateEOAFlavored(rng, flavors, false)
}

// GenerateEOAFlavoredLean is GenerateEOAFlavored without the derived-hash
// keccaks (AddrHash + delegation CodeHash) — the RNG draw sequence is
// byte-identical (neither keccak consumes draws); Code is still built (the
// erigon writer needs the bytes and re-derives the hash on its parallel encode
// workers), but AddrHash / CodeHash / StateAccount.CodeHash are left at their
// zero values. Only for writers that never read those fields.
func GenerateEOAFlavoredLean(rng *mrand.Rand, flavors EOAFlavors) *entitygen.Account {
	return generateEOAFlavored(rng, flavors, true)
}

func generateEOAFlavored(rng *mrand.Rand, flavors EOAFlavors, skipDerivedHashes bool) *entitygen.Account {
	var acc *entitygen.Account
	if skipDerivedHashes {
		acc = entitygen.GenerateEOALean(rng)
	} else {
		acc = entitygen.GenerateEOA(rng)
	}

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
		target := delegationTargets[rng.Intn(len(delegationTargets))]
		code := make([]byte, 0, 23)
		code = append(code, delegationPrefix...)
		code = append(code, target[:]...)
		acc.Code = code
		if !skipDerivedHashes {
			codeHash := crypto.Keccak256Hash(code)
			acc.CodeHash = codeHash
			acc.StateAccount.CodeHash = codeHash.Bytes()
		}
	}

	return acc
}
