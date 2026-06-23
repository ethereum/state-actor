//go:build cgo_erigon_commitment

package commitment

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	erigoncommon "github.com/erigontech/erigon/common"
	erigonkv "github.com/erigontech/erigon/db/kv"
	erigoncommitment "github.com/erigontech/erigon/execution/commitment"

	"github.com/ethereum/state-actor/internal/streamsort"
)

// erigonHash converts a geth-style 32-byte hash to Erigon's equivalent
// type. Both are `[32]byte`; this is a free byte-by-byte copy.
func erigonHash(h gethcommon.Hash) erigoncommon.Hash {
	var out erigoncommon.Hash
	copy(out[:], h[:])
	return out
}

// Account is the state-actor-facing input shape for one alloc entry.
// Used by EncodeAccountUpdate to produce the bytes the orchestrator
// writes into the commitmentInputStore during the streaming autofill
// loop. NOT consumed directly by ComputeGenesisRoot anymore —
// ComputeGenesisRoot reads encoded Update bytes from the
// commitmentInputStore via streamsort.Get.
type Account struct {
	Address gethcommon.Address
	Nonce   uint64
	Balance *uint256.Int
	Code    []byte
	Storage map[gethcommon.Hash]gethcommon.Hash
}

// Result carries the outputs of a successful commitment computation.
type Result struct {
	Root        gethcommon.Hash
	BranchNodes map[string][]byte
	HPHState    []byte
}

// EncodeAccountUpdate returns the Update.Encode bytes for an account
// (nonce + balance + codeHash). Callers Put this into the
// commitmentInputStore keyed by plain 20-byte address. ctx.Account
// later Decodes it back into an erigoncommitment.Update.
//
// Splits cleanly from the snapshot SerialiseV3 encoding (Update.Encode
// is HPH's internal wire format; SerialiseV3 is Erigon's state-domain
// .kv value format — different shapes).
func EncodeAccountUpdate(nonce uint64, balance *uint256.Int, code []byte) []byte {
	upd := erigoncommitment.Update{
		Flags: erigoncommitment.NonceUpdate | erigoncommitment.BalanceUpdate,
		Nonce: nonce,
	}
	if balance != nil {
		upd.Balance = *balance
	}
	if len(code) > 0 {
		h := crypto.Keccak256Hash(code)
		upd.CodeHash = erigonHash(h)
		upd.Flags |= erigoncommitment.CodeUpdate
	} else {
		upd.CodeHash = erigonHash(emptyCodeHash)
	}
	var numBuf [binary.MaxVarintLen64]byte
	return upd.Encode(nil, numBuf[:])
}

// EncodeStorageUpdate returns the Update.Encode bytes for one storage
// slot value. The value is LEFT-aligned into Update.Storage[0:len]
// (matching Erigon's TouchStorage invariant at commitment.go:1746-1753).
//
// Callers Put this into commitmentInputStore keyed by addr(20)||slot(32).
// An all-zero value should NOT be encoded — caller filters out.
func EncodeStorageUpdate(value []byte) []byte {
	trimmed := trimLeadingZeros(value)
	upd := erigoncommitment.Update{
		Flags:      erigoncommitment.StorageUpdate,
		StorageLen: int8(len(trimmed)),
	}
	copy(upd.Storage[:], trimmed)
	var numBuf [binary.MaxVarintLen64]byte
	return upd.Encode(nil, numBuf[:])
}

// ComputeGenesisRoot runs Erigon's ConcurrentPatriciaHashed (16-nibble
// subtree-parallel HPH) against the commitmentInputStore's encoded Update
// payloads.
//
// The caller is responsible for having populated commitmentInputStore
// during the buildAllocMap/writeSnapshots streaming loop: every alloc
// account writes one entry keyed by 20-byte addr; every non-zero
// storage slot writes one entry keyed by addr||slot. Encoding is done
// via EncodeAccountUpdate / EncodeStorageUpdate above.
//
// Parallelism (Phase 2 worker pool):
//   - upstream's NewUpdates with SetConcurrentCommitment(true) routes
//     Touch...Direct into 16 per-nibble etl.Collector instances (sharded
//     by hashedKey[0]).
//   - NewConcurrentPatriciaHashed spawns 16 SubTrie HPH instances, one
//     per first-nibble, all mounted under a single root HPH.
//   - Process dispatches to ParallelHashSort which runs 16 worker
//     goroutines via errgroup. Each worker drains its nibble's
//     collector, calls followAndUpdate on its SubTrie, then foldNibble
//     merges the subtree's final cell into root.grid[0][nib] under
//     rootMu. After all 16 finish, root.fold() produces the final hash
//     from the 16 child cells via the standard foldBranch path.
//   - Subtree branch keys are disjoint by first nibble — workers write
//     PutBranch into PER-WORKER context maps; closeFn merges into the
//     shared mergedBranches under mergeMu (no overwrite ambiguity).
//
// Memory profile: the in-memory `state map` of the original
// implementation is GONE — Account/Storage lookups during the HPH walk
// hit Pebble via streamsort.Get. Pebble's read path is thread-safe so
// 16 workers reading concurrently is fine. `mergedBranches` stays in
// memory: bounded by trie depth × entry count (~few hundred MB max
// even at 25 GB scale, since branches are O(N) not O(StorageSlots)).
func ComputeGenesisRoot(commitmentInputStore *streamsort.Store) (Result, error) {
	mergedBranches := make(map[string][]byte)
	var mergeMu sync.Mutex

	// factory yields a fresh subtreeCtx per worker — each owns a private
	// branches map. closeFn merges that map into mergedBranches when the
	// worker finishes. Upstream's ParallelHashSort calls factory both
	// during unfoldRoot (16 initial per-mount ctxs, populated with the
	// empty initial branches) and per-worker (16 fresh ctxs that absorb
	// followAndUpdate PutBranch writes). Both lifecycle paths funnel
	// through the same merge.
	factory := func() (erigoncommitment.PatriciaContext, func()) {
		sub := &subtreeCtx{
			commitmentInputStore: commitmentInputStore,
			branches:             make(map[string][]byte),
		}
		closeFn := func() {
			if len(sub.branches) == 0 {
				return
			}
			mergeMu.Lock()
			for k, v := range sub.branches {
				mergedBranches[k] = v
			}
			mergeMu.Unlock()
		}
		return sub, closeFn
	}

	// rootCtx is the context attached to the root HPH itself (consulted
	// by unfoldRoot's needUnfolding/unfold walk and by the final
	// root.fold() pass that builds the depth-0 branch). For a genesis
	// trie the initial branches map is empty so unfoldRoot is a no-op;
	// the root-level PutBranch from foldBranch lands here, then merges
	// via rootClose.
	rootCtx, rootClose := factory()
	defer rootClose()

	upds := erigoncommitment.NewUpdates(
		erigoncommitment.ModeDirect,
		"",
		erigoncommitment.KeyToHexNibbleHash,
	)
	// Force the parallel path on the very first Process call. Upstream's
	// idiomatic pipeline runs the first Process sequentially to populate
	// the root branch, then SetConcurrentCommitment(true) for subsequent
	// calls. For state-actor's one-shot genesis we don't have a
	// "subsequent" — we set the flag up-front. ParallelHashSort
	// (hex_concurrent_patricia_hashed.go:207) only requires
	// mode==ModeDirect && sortPerNibble==true — both satisfied. The
	// CanDoConcurrentNext gate is a next-call optimization hint, not a
	// correctness gate.
	upds.SetConcurrentCommitment(true)

	// Walker: iterate every entry in commitmentInputStore (which holds
	// addresses + addr||slot composite keys for the full alloc). Per
	// upstream commitment.go:1666-1681, ModeDirect's TouchPlainKeyDirect
	// discards the *Update arg — only the (hashedKey, plainKey) pair is
	// recorded in the per-nibble etl.Collector. So we pass a placeholder;
	// HPH re-fetches via ctx.Account/Storage during Process.
	var placeholder erigoncommitment.Update
	if err := commitmentInputStore.Iterate(func(plainKey, _ []byte) error {
		upds.TouchPlainKeyDirect(string(plainKey), &placeholder)
		return nil
	}); err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: iterate commitmentInputStore: %w", err)
	}

	hph := erigoncommitment.NewHexPatriciaHashed(20 /* accountKeyLen */, rootCtx)
	pph := erigoncommitment.NewConcurrentPatriciaHashed(hph, rootCtx)
	defer pph.Close()
	rootBytes, err := pph.Process(
		context.Background(),
		upds,
		"state-actor-genesis",
		nil,
		erigoncommitment.WarmupConfig{CtxFactory: factory},
	)
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: Process: %w", err)
	}
	if len(rootBytes) != 32 {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: unexpected root hash length %d", len(rootBytes))
	}
	var root gethcommon.Hash
	copy(root[:], rootBytes)

	// Flush the root HPH's deferred branch updates into rootCtx.PutBranch.
	//
	// NewHexPatriciaHashed defaults branchEncoder to deferred mode
	// (hex_patricia_hashed.go:160 upstream). SpawnSubTrie explicitly
	// disables deferred mode for the per-nibble mounts
	// (hex_concurrent_patricia_hashed.go:128 upstream), but the root
	// keeps it. The serial HexPatriciaHashed.Process flushes deferred
	// updates at its tail (hex_patricia_hashed.go:2889) — but
	// ConcurrentPatriciaHashed.ParallelHashSort
	// (hex_concurrent_patricia_hashed.go:207-295) returns rootHash
	// without that flush. Without this explicit call, the
	// mergedBranches map is missing the root-level (empty-prefix,
	// compact-key 0x10) branch entry, which causes a divergent root
	// vs serial HPH at any alloc that produces a 16-cell root branch
	// and a daemon FCU panic when SeekCommitment restores HPH state
	// and the first block-0 update calls ctx.Branch(nil).
	if err := pph.RootTrie().ApplyAndClearInlineDeferredUpdates(); err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: ApplyAndClearInlineDeferredUpdates: %w", err)
	}

	// HPHState comes from the root trie after all subtrees have folded
	// back into root.grid[0]. RootTrie() returns the same root HPH we
	// constructed above.
	hphState, err := pph.RootTrie().EncodeCurrentState(nil)
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: EncodeCurrentState: %w", err)
	}

	return Result{Root: root, BranchNodes: mergedBranches, HPHState: hphState}, nil
}

// subtreeCtx implements erigoncommitment.PatriciaContext over a
// streamsort-backed commitmentInputStore (random-access via Pebble,
// thread-safe read path) plus a PER-WORKER branches map.
//
// Lifecycle: one subtreeCtx per ConcurrentPatriciaHashed worker (16 of
// them inside ParallelHashSort) plus one for the root HPH. The
// per-worker branches map is mutated only by that worker's
// followAndUpdate PutBranch calls; on worker exit, the closeFn
// returned by the factory merges it into a shared mergedBranches map
// under mergeMu. Subtree branch keys are disjoint by first nibble so
// the post-merge has no overwrite ambiguity.
type subtreeCtx struct {
	commitmentInputStore *streamsort.Store
	branches             map[string][]byte
}

func (c *subtreeCtx) Branch(prefix []byte) ([]byte, erigonkv.Step, error) {
	if data, ok := c.branches[string(prefix)]; ok {
		return data, 0, nil
	}
	return nil, 0, nil
}

func (c *subtreeCtx) PutBranch(prefix []byte, data []byte, prevData []byte) error {
	c.branches[string(prefix)] = append([]byte(nil), data...)
	return nil
}

func (c *subtreeCtx) Account(plainKey []byte) (*erigoncommitment.Update, error) {
	enc, err := c.commitmentInputStore.Get(plainKey)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Account: Get(%x): %w", plainKey, err)
	}
	if enc == nil {
		u := new(erigoncommitment.Update)
		u.Flags = erigoncommitment.DeleteUpdate
		return u, nil
	}
	var u erigoncommitment.Update
	pos, err := u.Decode(enc, 0)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Account: decode plainKey=%x: %w", plainKey, err)
	}
	if pos != len(enc) {
		return nil, fmt.Errorf("commitment.subtreeCtx.Account: trailing bytes after decode")
	}
	if u.Flags&erigoncommitment.StorageUpdate != 0 {
		return nil, errors.New("commitment.subtreeCtx.Account: read storage entry via Account()")
	}
	return &u, nil
}

func (c *subtreeCtx) Storage(plainKey []byte) (*erigoncommitment.Update, error) {
	enc, err := c.commitmentInputStore.Get(plainKey)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Storage: Get(%x): %w", plainKey, err)
	}
	if enc == nil {
		u := new(erigoncommitment.Update)
		u.Flags = erigoncommitment.DeleteUpdate
		return u, nil
	}
	var u erigoncommitment.Update
	pos, err := u.Decode(enc, 0)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Storage: decode plainKey=%x: %w", plainKey, err)
	}
	if pos != len(enc) {
		return nil, fmt.Errorf("commitment.subtreeCtx.Storage: trailing bytes after decode")
	}
	return &u, nil
}

var emptyCodeHash = gethcommon.Hash{
	0xc5, 0xd2, 0x46, 0x01, 0x86, 0xf7, 0x23, 0x3c,
	0x92, 0x7e, 0x7d, 0xb2, 0xdc, 0xc7, 0x03, 0xc0,
	0xe5, 0x00, 0xb6, 0x53, 0xca, 0x82, 0x27, 0x3b,
	0x7b, 0xfa, 0xd8, 0x04, 0x5d, 0x85, 0xa4, 0x70,
}

func trimLeadingZeros(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}
