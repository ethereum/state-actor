//go:build cgo_reth

package reth

import (
	mrand "math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/internal/entitygen"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// TestStreaming_GoldenEqualsLegacy is the load-bearing determinism test
// for the upcoming RunCgo Phase 4 refactor.
//
// The legacy path generates N EOAs into a slice, then ComputeStateRoot
// sorts and feeds the HashBuilder. The streaming path generates the SAME
// sequence (identical seed, identical RNG draw count and order, just split
// across batches), RLP-encodes each account, writes (AddrHash → RLP) to
// the Pebble-backed Sorter, then calls ComputeStateRootStreaming(Iterate).
//
// The two roots MUST be byte-identical. Any reordering of GenerateEOA
// calls in the upcoming refactor will surface as a mismatch here.
func TestStreaming_GoldenEqualsLegacy(t *testing.T) {
	const seed, n = int64(42), 10_000
	const batchSize = 1_000

	// Legacy: one big slice, single ComputeStateRoot call.
	rngLegacy := mrand.New(mrand.NewSource(seed))
	legacy := make([]*entitygen.Account, n)
	for i := range legacy {
		legacy[i] = entitygen.GenerateEOA(rngLegacy)
	}
	legacyRoot, err := ComputeStateRoot(legacy, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}

	// Streaming: same seed, batched, RLP → Sorter → ComputeStateRootStreaming.
	sorter, err := streamsort.New(t.TempDir())
	if err != nil {
		t.Fatalf("streamsort.New: %v", err)
	}
	defer sorter.Close()

	rngStream := mrand.New(mrand.NewSource(seed))
	for produced := 0; produced < n; {
		b := batchSize
		if n-produced < b {
			b = n - produced
		}
		for i := 0; i < b; i++ {
			acc := entitygen.GenerateEOA(rngStream)
			rlpBytes, err := rlp.EncodeToBytes(acc.StateAccount)
			if err != nil {
				t.Fatalf("RLP encode: %v", err)
			}
			if err := sorter.Put(acc.AddrHash[:], rlpBytes); err != nil {
				t.Fatalf("sorter.Put: %v", err)
			}
		}
		produced += b
	}
	streamRoot, err := ComputeStateRootStreaming(sorter.Iterate, nil)
	if err != nil {
		t.Fatalf("ComputeStateRootStreaming: %v", err)
	}

	if legacyRoot != streamRoot {
		t.Fatalf("streaming root must equal legacy root for same seed:\n  legacy = %s\n  stream = %s",
			legacyRoot.Hex(), streamRoot.Hex())
	}
}

// TestStreaming_MixedAccountsContracts exercises the EOA-then-contract RNG
// pattern that RunCgo Phase 4b/4c will follow: all EOAs drawn first, then
// all contracts, with storage-root and code-hash computed per contract.
//
// This guards against batch-ordering bugs where contracts could end up
// interleaved with EOAs, or where storage-root/code-hash mutations on the
// contract's StateAccount happen at the wrong time relative to RLP-encoding
// for the sorter.
func TestStreaming_MixedAccountsContracts(t *testing.T) {
	const (
		seed       = int64(99)
		nEOAs      = 500
		nContracts = 50
		codeSize   = 256
		slotCount  = 5
		batchSize  = 100
	)

	// --- Legacy path (uninterrupted RNG stream) ---
	rngLegacy := mrand.New(mrand.NewSource(seed))
	legacy := make([]*entitygen.Account, 0, nEOAs+nContracts)
	for i := 0; i < nEOAs; i++ {
		legacy = append(legacy, entitygen.GenerateEOA(rngLegacy))
	}
	for i := 0; i < nContracts; i++ {
		c := entitygen.GenerateContract(rngLegacy, codeSize, slotCount)
		// Mirror the WriteContracts mutation: the contract's storage root
		// and code hash are filled in BEFORE the StateAccount is RLP-encoded
		// for the global state trie. Both paths must apply the same
		// mutation in the same order.
		root, err := computeStorageRoot(c.Storage, nil)
		if err != nil {
			t.Fatalf("legacy computeStorageRoot: %v", err)
		}
		c.StateAccount.Root = root
		c.StateAccount.CodeHash = crypto.Keccak256Hash(c.Code).Bytes()
		legacy = append(legacy, c)
	}
	legacyRoot, err := ComputeStateRoot(legacy, nil)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}

	// --- Streaming path (same seed, batched, sorter-backed) ---
	sorter, err := streamsort.New(t.TempDir())
	if err != nil {
		t.Fatalf("streamsort.New: %v", err)
	}
	defer sorter.Close()

	rngStream := mrand.New(mrand.NewSource(seed))

	// 4b — synthetic EOAs in batches.
	remaining := nEOAs
	for remaining > 0 {
		b := batchSize
		if remaining < b {
			b = remaining
		}
		for i := 0; i < b; i++ {
			acc := entitygen.GenerateEOA(rngStream)
			rlpBytes, err := rlp.EncodeToBytes(acc.StateAccount)
			if err != nil {
				t.Fatalf("RLP encode EOA: %v", err)
			}
			if err := sorter.Put(acc.AddrHash[:], rlpBytes); err != nil {
				t.Fatalf("sorter.Put: %v", err)
			}
		}
		remaining -= b
	}

	// 4c — synthetic contracts in batches with per-contract storage-root +
	// code-hash mutation, exactly like WriteContracts will perform in the
	// refactored RunCgo (mutate-in-place, then RLP-encode for the sorter).
	remaining = nContracts
	for remaining > 0 {
		b := batchSize
		if remaining < b {
			b = remaining
		}
		batch := make([]*entitygen.Account, b)
		for i := 0; i < b; i++ {
			batch[i] = entitygen.GenerateContract(rngStream, codeSize, slotCount)
		}
		for _, c := range batch {
			root, err := computeStorageRoot(c.Storage, nil)
			if err != nil {
				t.Fatalf("streaming computeStorageRoot: %v", err)
			}
			c.StateAccount.Root = root
			c.StateAccount.CodeHash = crypto.Keccak256Hash(c.Code).Bytes()
			rlpBytes, err := rlp.EncodeToBytes(c.StateAccount)
			if err != nil {
				t.Fatalf("RLP encode contract: %v", err)
			}
			if err := sorter.Put(c.AddrHash[:], rlpBytes); err != nil {
				t.Fatalf("sorter.Put contract: %v", err)
			}
		}
		remaining -= b
	}

	streamRoot, err := ComputeStateRootStreaming(sorter.Iterate, nil)
	if err != nil {
		t.Fatalf("ComputeStateRootStreaming: %v", err)
	}

	if legacyRoot != streamRoot {
		t.Fatalf("mixed EOAs+contracts root mismatch:\n  legacy = %s\n  stream = %s",
			legacyRoot.Hex(), streamRoot.Hex())
	}
}
