//go:build cgo_erigon_commitment

package commitment

import (
	"fmt"

	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/streamsort"
)

// ComputeGenesisRootFromAccounts is a test-only convenience wrapper for
// small in-memory inputs (the commitment unit tests + the H4 invariance
// proof). Materialises the slice into a temp streamsort + calls the
// streaming ComputeGenesisRoot. Not for production at bench scale.
//
// Relocated here from commitment.go: it had no non-test callers, so it
// lives in a _test.go file (still under cgo_erigon_commitment) to keep it
// out of the production build while remaining available to its tests.
func ComputeGenesisRootFromAccounts(accounts []Account) (Result, error) {
	store, err := streamsort.New("")
	if err != nil {
		return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: streamsort.New: %w", err)
	}
	defer store.Close()

	for _, a := range accounts {
		// Account entry keyed by 20-byte address.
		var balance *uint256.Int
		if a.Balance != nil {
			balance = a.Balance
		}
		acctBytes := EncodeAccountUpdate(a.Nonce, balance, a.Code)
		if err := store.Put(a.Address[:], acctBytes); err != nil {
			return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: put account %s: %w", a.Address.Hex(), err)
		}
		// Storage entries keyed by addr||slot. Skip all-zero values.
		for slot, val := range a.Storage {
			trimmed := trimLeadingZeros(val[:])
			if len(trimmed) == 0 {
				continue
			}
			composite := make([]byte, 0, 52)
			composite = append(composite, a.Address[:]...)
			composite = append(composite, slot[:]...)
			storBytes := EncodeStorageUpdate(val[:])
			if err := store.Put(composite, storBytes); err != nil {
				return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: put storage %s/%s: %w", a.Address.Hex(), slot.Hex(), err)
			}
		}
	}
	// ComputeGenesisRoot requires its input streamsort to be Finalized
	// — Iterate and Get on the store both gate on the Finalize state
	// transition. The wrapper Puts everything here, so we Finalize
	// before delegating.
	if err := store.Finalize(); err != nil {
		return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: Finalize: %w", err)
	}
	return ComputeGenesisRoot(store)
}
