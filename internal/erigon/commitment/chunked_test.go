//go:build cgo_erigon_commitment

package commitment

import (
	"bytes"
	"encoding/binary"
	"testing"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// TestComputeGenesisRoot_ChunkedVsSingle is the A0 correctness gate: it
// asserts that building the commitment trie in MANY small incremental
// chunks (forced via a tiny commitmentChunkKeys) yields the byte-identical
// root + branch set as building it in a single Process. If incremental
// chunking is not equivalent (wrong state carry, deferred-update timing,
// concurrent-incremental bug), the roots diverge and this fails.
//
// NOTE: like the rest of this package it links against erigon, so it runs
// on Docker/CI/bench, not on macOS (duplicate secp256k1). It is the primary
// validation for the multi-chunk path that production only exercises at
// bench scale.
func TestComputeGenesisRoot_ChunkedVsSingle(t *testing.T) {
	// Gate for A0: forcing a tiny chunk size (serial incremental path) must
	// yield the byte-identical root + branch set as the single concurrent
	// Process. The earlier CONCURRENT incremental attempt failed here
	// ("empty branch data read during unfold ... leaf→branch"); the serial
	// per-chunk engine (upstream's proven per-block model) fixes it.
	//
	// Alloc with enough commitment keys (accounts + storage) to span many
	// chunks at chunk size 3.
	accounts := make([]Account, 0, 12)
	for i := 0; i < 12; i++ {
		var addr gethcommon.Address
		addr[0] = byte(i + 1)
		a := Account{
			Address: addr,
			Nonce:   uint64(i + 1),
			Balance: uint256.NewInt(uint64(1000 * (i + 1))),
		}
		if i%2 == 0 { // half the accounts carry 3 storage slots each
			a.Storage = map[gethcommon.Hash]gethcommon.Hash{}
			for j := 0; j < 3; j++ {
				var k, v gethcommon.Hash
				k[0], k[31] = byte(i), byte(j+1)
				v[31] = byte(j + 7)
				a.Storage[k] = v
			}
		}
		accounts = append(accounts, a)
	}

	compute := func(chunk int) Result {
		t.Helper()
		restore := setCommitmentChunkKeys(chunk)
		defer restore()
		res, err := ComputeGenesisRootFromAccounts(accounts)
		if err != nil {
			t.Fatalf("ComputeGenesisRootFromAccounts(chunk=%d): %v", chunk, err)
		}
		return res
	}

	single := compute(1 << 30) // one Process
	chunked := compute(3)      // ~10 Process calls

	if single.Root != chunked.Root {
		t.Fatalf("ROOT DIVERGED: single=%s chunked=%s — incremental chunking is NOT equivalent",
			single.Root.Hex(), chunked.Root.Hex())
	}
	if single.BranchCount != chunked.BranchCount {
		t.Errorf("branch count differs: single=%d chunked=%d", single.BranchCount, chunked.BranchCount)
	}
	if !bytes.Equal(branchNodesBytes(single.BranchNodes), branchNodesBytes(chunked.BranchNodes)) {
		t.Errorf("branch BYTES differ between single-shot and chunked builds")
	}
	t.Logf("chunked == single: root %s, %d branches", single.Root.Hex(), single.BranchCount)
}

// TestComputeGenesisRoot_FirstChunkShrink is the correctness gate for the
// serial-first-chunk shrink: a first chunk SMALLER than the regular chunk size
// must yield the byte-identical root — PROVIDED it still covers all 16 first
// nibbles (the concurrent unfold needs every first-nibble root child to exist;
// a sub-coverage first chunk legitimately diverges). 2048 keccak-distributed
// accounts make even a 256-key first chunk cover all 16 nibbles.
func TestComputeGenesisRoot_FirstChunkShrink(t *testing.T) {
	const n = 2048
	accounts := make([]Account, 0, n)
	for i := 0; i < n; i++ {
		var addr gethcommon.Address
		binary.BigEndian.PutUint64(addr[12:], uint64(i+1))
		accounts = append(accounts, Account{
			Address: addr,
			Nonce:   uint64(i + 1),
			Balance: uint256.NewInt(uint64(i + 1)),
		})
	}
	compute := func(chunk, first int) Result {
		t.Helper()
		r1 := setCommitmentChunkKeys(chunk)
		defer r1()
		r2 := setFirstChunkKeys(first)
		defer r2()
		res, err := ComputeGenesisRootFromAccounts(accounts)
		if err != nil {
			t.Fatalf("ComputeGenesisRootFromAccounts(chunk=%d,first=%d): %v", chunk, first, err)
		}
		return res
	}
	single := compute(0, 1<<17)  // single concurrent Process (production default)
	shrunk := compute(1024, 256) // first chunk 256 (covers all 16 nibbles), rest 1024
	if single.Root != shrunk.Root {
		t.Fatalf("ROOT DIVERGED: single=%s shrunk-first-chunk=%s — nibble-covering first-chunk shrink changed the root",
			single.Root.Hex(), shrunk.Root.Hex())
	}
	t.Logf("first-chunk-shrink == single: root %s", single.Root.Hex())
}
