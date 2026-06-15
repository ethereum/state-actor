package reth

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// computeStorageRoot computes the MPT root over the contract's storage slots.
//
// Leaves are keccak(slot_key)-sorted; each value is RLP-encoded with
// leading-zero stripping (Ethereum convention). Empty storage returns the
// canonical empty-MPT root 0x56e81f17...
//
// This mirrors alloy_trie::root::storage_root_unhashed exactly:
//   - key: keccak256(raw_slot_key), nibble-unpacked
//   - value: RLP(trim_left_zeros(raw_32_byte_value))
//   - sorted by hashed key ascending
//
// Zero-valued slots are skipped, matching alloy-genesis's From<GenesisAccount>
// for TrieAccount which filters `.filter(|(_, value)| !value.is_zero())`.
// (GenerateContract already guarantees non-zero values; this guard is present
// for correctness when called with arbitrary slot sets.)
//
// emit is the per-branch-node callback. Pass nil for compute-only (no
// StoragesTrie persistence). Pass a non-nil callback when populating
// reth's StoragesTrie for an account at genesis — the caller is expected
// to forward the (path, BranchNodeCompact) tuple into MDBX under the
// DupSort key `keccak(address) || StoredNibblesSubKey(path)`.
func computeStorageRoot(slots []entitygen.StorageSlot, emit iReth.NodeEmitter) (common.Hash, error) {
	if len(slots) == 0 {
		// Canonical empty storage trie root: keccak256(rlp([])).
		return common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"), nil
	}

	// Sort by keccak(slot_key) ascending.
	type hashedSlot struct {
		keyHash common.Hash
		value   common.Hash
	}
	sorted := make([]hashedSlot, 0, len(slots))
	for _, s := range slots {
		if s.Value == (common.Hash{}) {
			// Skip zero-valued slots: they represent deletions and must not
			// appear in the trie. alloy-genesis does the same.
			continue
		}
		sorted = append(sorted, hashedSlot{
			keyHash: crypto.Keccak256Hash(s.Key[:]),
			value:   s.Value,
		})
	}
	if len(sorted) == 0 {
		return common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"), nil
	}

	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].keyHash[:], sorted[j].keyHash[:]) < 0
	})

	hb := newAccountTrieBuilder(emit)

	for _, s := range sorted {
		// RLP-encode the value bytes with leading zeros stripped.
		valBytes := s.value[:]
		for len(valBytes) > 0 && valBytes[0] == 0 {
			valBytes = valBytes[1:]
		}
		valRLP, err := rlp.EncodeToBytes(valBytes)
		if err != nil {
			return common.Hash{}, fmt.Errorf("computeStorageRoot: rlp encode slot value: %w", err)
		}
		// addrHashToNibbles works for any 32-byte hash key, not just account hashes.
		if err := hb.AddLeaf(addrHashToNibbles(s.keyHash[:]), valRLP); err != nil {
			return common.Hash{}, fmt.Errorf("computeStorageRoot: AddLeaf: %w", err)
		}
	}
	return hb.Root(), nil
}
