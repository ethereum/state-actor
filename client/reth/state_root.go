package reth

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// ComputeStateRoot returns the MPT state root over the supplied accounts.
//
// Each account's StateAccount is RLP-encoded and fed (in keccak-sorted-by-
// AddrHash order) to a HashBuilder. The returned root matches what
// go-ethereum's trie.StackTrie computes for the same inputs.
//
// Accounts must have AddrHash already populated; entitygen sets this
// when generating EOAs/contracts.
//
// emit is the per-branch-node callback. Pass nil for compute-only (existing
// behavior, no trie-table persistence). Pass a non-nil callback to populate
// reth's AccountsTrie — see ComputeStateRootStreaming's docstring for the
// emission contract and why it's needed for bench-scale DBs.
//
// Empty input returns the canonical empty-MPT hash (HashBuilder's empty case).
func ComputeStateRoot(accounts []*entitygen.Account, emit iReth.NodeEmitter) (common.Hash, error) {
	sorted := make([]*entitygen.Account, len(accounts))
	copy(sorted, accounts)
	sortAccountsByAddrHash(sorted)

	hb := newAccountTrieBuilder(emit)

	for _, acc := range sorted {
		if acc.StateAccount == nil {
			return common.Hash{}, fmt.Errorf("ComputeStateRoot: account at addr %s has nil StateAccount", acc.Address.Hex())
		}
		rlpBytes, err := rlp.EncodeToBytes(acc.StateAccount)
		if err != nil {
			return common.Hash{}, fmt.Errorf("ComputeStateRoot: rlp encode %s: %w", acc.Address.Hex(), err)
		}
		nibbles := addrHashToNibbles(acc.AddrHash[:])
		if err := hb.AddLeaf(nibbles, rlpBytes); err != nil {
			return common.Hash{}, fmt.Errorf("ComputeStateRoot: AddLeaf %s: %w", acc.Address.Hex(), err)
		}
	}

	return hb.Root(), nil
}

// ComputeStateRootStreaming returns the MPT state root from a sorted-by-key
// stream of (addrHash, accountRLP) pairs. The supplied iter callback is
// invoked exactly once and is expected to call yield for each pair in
// ascending addrHash order — the HashBuilder enforces that invariant.
//
// This is the streaming counterpart of ComputeStateRoot used by RunCgo
// Phase 4 to drain a Sorter (Pebble auto-sorts on iterate) without holding
// every account in RAM. The resulting root is byte-identical to what
// ComputeStateRoot would produce over the same set, given the same
// sort-by-AddrHash order.
//
// emit is the per-branch-node callback:
//   - nil → compute-only mode (back-compat for callers that only want the
//     root hash; no trie-table persistence).
//   - non-nil → full-emissions mode: every branch with RLP ≥ 32 bytes is
//     forwarded to emit in lexicographic path order. The caller is expected
//     to persist these into reth's AccountsTrie table (via MDBX
//     cursor.Append for the sequential-write fast-path).
//
// Without persisted AccountsTrie rows, reth's payload-builder state-root
// computation falls back to a linear walk of HashedAccounts (and
// HashedStorages per leaf) on every block, exceeding MDBX's 300 s read-txn
// timeout on 100+ GB DBs. See project_reth_trie_cache.md memory for the
// motivation.
//
// Memory bound: O(trie depth * 33 bytes) ≈ 2 KB regardless of how many
// pairs the iterator emits. HashBuilder.AddLeaf copies its inputs, so the
// caller's slices (e.g. Pebble's iter.Key()/iter.Value() which alias
// internal buffers) can be safely reused after each yield call.
func ComputeStateRootStreaming(iter func(yield func(addrHash, accountRLP []byte) error) error, emit iReth.NodeEmitter) (common.Hash, error) {
	hb := newAccountTrieBuilder(emit)

	err := iter(func(addrHash, accountRLP []byte) error {
		nibbles := addrHashToNibbles(addrHash)
		if err := hb.AddLeaf(nibbles, accountRLP); err != nil {
			return fmt.Errorf("AddLeaf: %w", err)
		}
		return nil
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("ComputeStateRootStreaming: iter: %w", err)
	}

	return hb.Root(), nil
}

// newAccountTrieBuilder returns a HashBuilder configured for the account
// trie. nil emit selects the alloy_trie-compatible constructor (compute-
// only, no on-disk emissions). A non-nil emit selects the
// full-emissions constructor so every ≥32-byte branch is persisted to
// AccountsTrie via the caller's MDBX cursor write.
func newAccountTrieBuilder(emit iReth.NodeEmitter) *iReth.HashBuilder {
	if emit == nil {
		return iReth.NewHashBuilder(func(iReth.StoredNibbles, iReth.BranchNodeCompact) error {
			return nil
		})
	}
	return iReth.NewHashBuilderFullEmissions(emit)
}

// sortAccountsByAddrHash sorts in place by AddrHash ascending.
func sortAccountsByAddrHash(accounts []*entitygen.Account) {
	sort.Slice(accounts, func(i, j int) bool {
		return bytes.Compare(accounts[i].AddrHash[:], accounts[j].AddrHash[:]) < 0
	})
}

// addrHashToNibbles unpacks each byte to two nibbles, high then low.
// Mirrors internal/reth's unexported bytesToNibbles helper; duplicated
// here to avoid widening internal/reth's public surface.
func addrHashToNibbles(b []byte) []byte {
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i] = c >> 4
		out[2*i+1] = c & 0x0f
	}
	return out
}
