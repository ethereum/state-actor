package trie

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	gethtrie "github.com/ethereum/go-ethereum/trie"

	"github.com/ethereum/state-actor/internal/neth"
)

// NodeStorage is the sink for the trie nodes Builder emits.
//
// SetStateNode is called once per state-trie node (account-tree side) with:
//
//   - path:    the 32-byte packed path representation (high nibble first).
//   - pathLen: nibble count of the path (0..64).
//   - keccak:  keccak256(rlp) of the node.
//   - rlp:     the node's full RLP bytes (already copied — Builder does the
//     deep-copy from StackTrie's volatile buffer).
//
// SetStorageNode is the same shape but tagged with the contract's addrHash
// (the keccak of the contract address). The sink maps (path, pathLen, and for
// storage the addrHash) to whatever node-key scheme its layout uses — for the
// Nethermind writer, internal/neth/flat's flat node CFs.
//
// Implementations may return an error to abort the build; Builder will
// propagate it from the next entry-point method (AddAccount, AddStorageSlot,
// FinalizeStorageRoot, FinalizeStateRoot).
type NodeStorage interface {
	SetStateNode(path []byte, pathLen int, keccak [32]byte, rlp []byte) error
	SetStorageNode(addrHash [32]byte, path []byte, pathLen int, keccak [32]byte, rlp []byte) error
}

// Builder wraps go-ethereum's trie.StackTrie and routes its
// OnTrieNode callbacks into a Nethermind-shaped NodeStorage sink.
//
// Two tries run in parallel:
//
//   - The account trie (accumulating across AddAccount calls).
//   - At most one storage trie at a time (accumulating across
//     AddStorageSlot calls, finalized by FinalizeStorageRoot before the
//     next AddAccount).
//
// Callers must:
//  1. For each account in addrHash-sorted order:
//     a. For each storage slot in slotKeyHash-sorted order:
//     AddStorageSlot(addrHash, slotKeyHash, valueRLP)
//     b. FinalizeStorageRoot(addrHash) → use as the account's storageRoot
//     c. AddAccount(addrHash, accountRLP_with_storageRoot)
//  2. FinalizeStateRoot() → state root.
//
// Empty-tree short-circuits:
//   - FinalizeStorageRoot with zero AddStorageSlot calls returns
//     neth.EmptyTreeHash without invoking SetStorageNode.
//   - FinalizeStateRoot with zero AddAccount calls returns
//     neth.EmptyTreeHash without invoking SetStateNode.
//
// Builder is single-goroutine. The geth StackTrie's callback fires on the
// goroutine that calls Update/Hash; Builder does not introduce concurrency.
type Builder struct {
	storage NodeStorage

	// First sink error; checked at every entry point so the call site
	// can surface a stable error from a later method even if the actual
	// failure was inside a callback.
	err error

	// Account trie state.
	accountTrie  *gethtrie.StackTrie
	accountCount int

	// Currently-active storage trie state. Reset by FinalizeStorageRoot.
	hasStorageTrie         bool
	currentStorageAddrHash [32]byte
	currentStorageTrie     *gethtrie.StackTrie
	currentStorageCount    int
}

// NewBuilder returns a Builder that emits node writes into storage.
func NewBuilder(storage NodeStorage) *Builder {
	b := &Builder{storage: storage}
	b.accountTrie = gethtrie.NewStackTrie(b.accountSink())
	return b
}

// AddStorageSlot appends one storage slot to the storage trie of addrHash.
// Slots must be supplied in slotKeyHash-ascending order. Values are the
// RLP-encoded slot value (typically a trimmed-leading-zeros uint).
//
// The first AddStorageSlot for a given addrHash opens a fresh storage
// trie; subsequent calls with the SAME addrHash extend it. Calling with a
// DIFFERENT addrHash before FinalizeStorageRoot is a usage error and
// returns an error.
func (b *Builder) AddStorageSlot(addrHash [32]byte, slotKeyHash [32]byte, valueRLP []byte) error {
	if b.err != nil {
		return b.err
	}
	if b.hasStorageTrie && b.currentStorageAddrHash != addrHash {
		return fmt.Errorf("trie.Builder: AddStorageSlot for new addrHash before FinalizeStorageRoot of previous (%x)", b.currentStorageAddrHash)
	}
	if !b.hasStorageTrie {
		b.currentStorageAddrHash = addrHash
		b.currentStorageTrie = gethtrie.NewStackTrie(b.storageSink(addrHash))
		b.currentStorageCount = 0
		b.hasStorageTrie = true
	}
	if err := b.currentStorageTrie.Update(slotKeyHash[:], valueRLP); err != nil {
		return fmt.Errorf("trie.Builder: storage Update: %w", err)
	}
	b.currentStorageCount++
	return b.err
}

// FinalizeStorageRoot returns the storage root of the per-account trie
// addressed by addrHash. Resets the per-account state so the next
// AddStorageSlot starts fresh.
//
// If no AddStorageSlot calls were made for addrHash, returns
// neth.EmptyTreeHash without calling SetStorageNode (matching Nethermind's
// short-circuit at NodeStorage.cs:107).
func (b *Builder) FinalizeStorageRoot(addrHash [32]byte) ([32]byte, error) {
	if b.err != nil {
		return [32]byte{}, b.err
	}
	if !b.hasStorageTrie || b.currentStorageAddrHash != addrHash {
		// No slots for this account → empty tree.
		return [32]byte(neth.EmptyTreeHash), nil
	}
	if b.currentStorageCount == 0 {
		// Trie was opened but never written to; treat as empty.
		b.hasStorageTrie = false
		b.currentStorageTrie = nil
		return [32]byte(neth.EmptyTreeHash), nil
	}
	root := b.currentStorageTrie.Hash()
	b.hasStorageTrie = false
	b.currentStorageTrie = nil
	b.currentStorageCount = 0
	return [32]byte(root), b.err
}

// AddAccount adds one account to the global state trie. addrHash is
// keccak(address); accountRLP is the byte-encoded account in
// `[nonce, balance, storageRoot, codeHash]` shape (typically produced by
// neth/rlp.EncodeAccount).
//
// Accounts must be supplied in addrHash-ascending order.
func (b *Builder) AddAccount(addrHash [32]byte, accountRLP []byte) error {
	if b.err != nil {
		return b.err
	}
	if b.hasStorageTrie {
		return errors.New("trie.Builder: AddAccount called with an open storage trie; FinalizeStorageRoot first")
	}
	if err := b.accountTrie.Update(addrHash[:], accountRLP); err != nil {
		return fmt.Errorf("trie.Builder: account Update: %w", err)
	}
	b.accountCount++
	return b.err
}

// FinalizeStateRoot returns the global state root.
//
// If no AddAccount calls were made, returns neth.EmptyTreeHash without
// calling SetStateNode. Useful for empty-alloc genesis chainspecs.
func (b *Builder) FinalizeStateRoot() ([32]byte, error) {
	if b.err != nil {
		return [32]byte{}, b.err
	}
	if b.accountCount == 0 {
		return [32]byte(neth.EmptyTreeHash), nil
	}
	root := b.accountTrie.Hash()
	return [32]byte(root), b.err
}

// accountSink returns the OnTrieNode callback that routes account-trie
// nodes into NodeStorage.SetStateNode. The path/blob slices StackTrie
// passes are volatile (reused across calls) — we deep-copy before
// handing to the sink.
func (b *Builder) accountSink() gethtrie.OnTrieNode {
	return func(path []byte, hash common.Hash, blob []byte) {
		if b.err != nil {
			return
		}
		p := make([]byte, len(path))
		copy(p, path)
		bl := make([]byte, len(blob))
		copy(bl, blob)

		// Convert the nibble path to the 32-byte packed form the flat
		// node-key encoders consume.
		packed := packNibblesTo32(p)

		var k [32]byte
		copy(k[:], hash[:])

		if err := b.storage.SetStateNode(packed[:], len(p), k, bl); err != nil {
			b.err = fmt.Errorf("SetStateNode: %w", err)
		}
	}
}

// storageSink returns the per-account OnTrieNode callback for the
// currently-active storage trie. addrHash is captured by closure so the
// sink can tag each node with the right account.
func (b *Builder) storageSink(addrHash [32]byte) gethtrie.OnTrieNode {
	return func(path []byte, hash common.Hash, blob []byte) {
		if b.err != nil {
			return
		}
		p := make([]byte, len(path))
		copy(p, path)
		bl := make([]byte, len(blob))
		copy(bl, blob)

		packed := packNibblesTo32(p)

		var k [32]byte
		copy(k[:], hash[:])

		if err := b.storage.SetStorageNode(addrHash, packed[:], len(p), k, bl); err != nil {
			b.err = fmt.Errorf("SetStorageNode: %w", err)
		}
	}
}

// packNibblesTo32 packs a nibble slice (each byte ∈ [0, 15]) into a
// 32-byte buffer (high nibble first per byte, zero-padded). Mirrors the
// representation `Nethermind.Trie.TreePath.Path.BytesAsSpan` exposes; the flat
// node-key encoders (internal/neth/flat) read the first 3 bytes (top nodes),
// the first 8 (shortened), or the full 32 (fallback) of this buffer.
func packNibblesTo32(nibbles []byte) [32]byte {
	var out [32]byte
	for i, n := range nibbles {
		if i/2 >= 32 {
			break
		}
		if i%2 == 0 {
			out[i/2] = n << 4
		} else {
			out[i/2] |= n & 0x0f
		}
	}
	return out
}
