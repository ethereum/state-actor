package entitygen

import (
	"bytes"
	mrand "math/rand"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// GenerateContract generates a contract account with code and storage using
// the supplied RNG.
//
// RNG draw order (NEVER reorder — golden hashes depend on it):
//  1. rng.Read(addr[:])             — 20 bytes for the address
//  2. rng.Intn(codeSize)             — extra code length (codeSize + extra = total)
//  3. rng.Read(code)                 — codeSize+extra bytes of code
//  4. rng.Intn(100)                  — balance multiplier (×1e18 wei)
//  5. for each of numSlots:
//       a. rng.Read(key[:])           — 32 bytes
//       b. rng.Read(value[:])         — 32 bytes (zero-valued bumped to 0x..01)
//  6. rng.Intn(1000)                  — nonce (after the slot loop)
//
// The returned Account.Storage is sorted by Key so consumers can stream into a
// StackTrie without re-sorting.
func GenerateContract(rng *mrand.Rand, codeSize int, numSlots int) *Account {
	// (Implementation follows the function-level doc above. The package-
	// level helper GenerateContractRoll is the preferred entry point for
	// callers in writer + test code; calling GenerateContract directly is
	// only correct when the slot count is *deliberately* a constant (e.g.
	// equivalence tests that don't reproduce a writer's RNG sequence).
	// See GenerateContractRoll.)

	var addr common.Address
	rng.Read(addr[:])

	// Generate random code
	totalCodeSize := codeSize + rng.Intn(codeSize)
	code := make([]byte, totalCodeSize)
	rng.Read(code)
	codeHash := crypto.Keccak256Hash(code)

	// Random balance
	balance := new(uint256.Int).Mul(
		uint256.NewInt(uint64(rng.Intn(100))),
		uint256.NewInt(1e18),
	)

	// Generate storage slots as a pre-sorted slice for deterministic trie insertion.
	storage := make([]StorageSlot, 0, numSlots)
	for j := 0; j < numSlots; j++ {
		var key, value common.Hash
		rng.Read(key[:])
		rng.Read(value[:])
		// Ensure value is non-zero (zero values are deletions)
		if value == (common.Hash{}) {
			value[31] = 1
		}
		storage = append(storage, StorageSlot{Key: key, Value: value})
	}
	sort.Slice(storage, func(i, j int) bool {
		return bytes.Compare(storage[i].Key[:], storage[j].Key[:]) < 0
	})

	return &Account{
		Address:  addr,
		AddrHash: crypto.Keccak256Hash(addr[:]),
		StateAccount: &types.StateAccount{
			Nonce:    uint64(rng.Intn(1000)),
			Balance:  balance,
			Root:     types.EmptyRootHash, // Will be computed
			CodeHash: codeHash.Bytes(),
		},
		Code:     code,
		CodeHash: codeHash,
		Storage:  storage,
	}
}

// GenerateContractRoll bundles the canonical "roll a slot count, then
// roll a contract" RNG sequence every state-actor writer uses. Splitting
// the two calls (GenerateSlotCount + GenerateContract) at every call
// site is what produced ethereum/state-actor#42's secondary bug —
// boot-test reproductions skipped GenerateSlotCount and ended up one
// Float64 ahead of the writer's RNG, so the contracts they reproduced
// did not match the contracts the writer persisted.
//
// All four client writers (besu, geth, nethermind, reth) call this in
// place of the two-step idiom. Test-side reproductions that need to
// re-derive the writer's contracts from the same seed call this too —
// it is the single source of truth for the contract draw order.
//
// Equivalent to:
//
//	numSlots := GenerateSlotCount(rng, dist, minSlots, maxSlots)
//	return GenerateContract(rng, codeSize, numSlots)
//
// Direct callers of GenerateContract still exist for tests that
// deliberately use a constant slot count (e.g. legacy-vs-streaming
// equivalence in client/reth/streaming_test.go) — those don't reproduce
// a writer's RNG sequence so GenerateSlotCount would just be noise.
func GenerateContractRoll(rng *mrand.Rand, dist Distribution, codeSize, minSlots, maxSlots int) *Account {
	numSlots := GenerateSlotCount(rng, dist, minSlots, maxSlots)
	return GenerateContract(rng, codeSize, numSlots)
}
