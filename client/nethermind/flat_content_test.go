//go:build cgo_neth

package nethermind

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/neth"
	"github.com/ethereum/state-actor/internal/neth/flat"
	nethrlp "github.com/ethereum/state-actor/internal/neth/rlp"
)

// TestFlatDBContents runs a tiny genesis alloc through the writer, reopens the
// flat column DB read-only, and asserts the flat Account/Storage leaf rows, the
// three Metadata markers, and that the trie node CFs are populated — directly.
//
// This is the fast (no-Docker) guard for the flat tee / CF-routing / marker
// glue. A bug that tees the wrong bytes into a flat leaf, or into the wrong CF,
// does NOT move the state root, so the golden/root tests cannot catch it — only
// reading the CFs back can. It complements TestNethGoldenStateRoot (trie side)
// and TestE2ESuite (booted Nethermind).
func TestFlatDBContents(t *testing.T) {
	eoa := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	contract := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	slotKey := common.HexToHash("0x01")
	slotVal := common.HexToHash("0x2a")
	code := []byte{0x60, 0x00, 0x60, 0x00, 0x00}

	accounts := map[common.Address]*types.StateAccount{
		eoa: {
			Nonce: 7, Balance: uint256.NewInt(1000),
			Root: common.Hash(neth.EmptyTreeHash), CodeHash: neth.OfAnEmptyString.Bytes(),
		},
		contract: {
			// Root is spliced from storage and CodeHash is set from the code by
			// the writer; initial values here don't matter.
			Nonce: 0, Balance: uint256.NewInt(0),
			Root: types.EmptyRootHash, CodeHash: neth.OfAnEmptyString.Bytes(),
		},
	}
	codes := map[common.Address][]byte{contract: code}
	storages := map[common.Address]map[common.Hash]common.Hash{
		contract: {slotKey: slotVal},
	}

	tmp := t.TempDir()
	dbs, err := openNethDBs(tmp)
	if err != nil {
		t.Fatalf("openNethDBs: %v", err)
	}
	var stats generator.Stats
	root, err := writeSyntheticAccounts(context.Background(), dbs, generator.Config{}, accounts, codes, storages, &stats)
	if err != nil {
		t.Fatalf("writeSyntheticAccounts: %v", err)
	}
	if err := writeFlatMetadata(dbs, root); err != nil {
		t.Fatalf("writeFlatMetadata: %v", err)
	}
	dbs.Close()

	// Reopen <tmp>/flat read-only with the full CF set.
	opts := grocksdb.NewDefaultOptions()
	defer opts.Destroy()
	cfOpts := make([]*grocksdb.Options, len(flat.ColumnNames))
	for i := range cfOpts {
		cfOpts[i] = opts
	}
	fdb, handles, err := grocksdb.OpenDbForReadOnlyColumnFamilies(
		opts, filepath.Join(tmp, "flat"), flat.ColumnNames, cfOpts, false)
	if err != nil {
		t.Fatalf("reopen flat db: %v", err)
	}
	defer fdb.Close()
	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()

	get := func(col flat.Column, key []byte) []byte {
		s, err := fdb.GetCF(ro, handles[col], key)
		if err != nil {
			t.Fatalf("GetCF %s: %v", flat.ColumnNames[col], err)
		}
		defer s.Free()
		return append([]byte(nil), s.Data()...)
	}
	cfCount := func(col flat.Column) int {
		it := fdb.NewIteratorCF(ro, handles[col])
		defer it.Close()
		n := 0
		for it.SeekToFirst(); it.Valid(); it.Next() {
			n++
		}
		return n
	}

	hash := func(b []byte) [32]byte {
		var h [32]byte
		copy(h[:], crypto.Keccak256(b))
		return h
	}

	// Account CF: both accounts present at keccak(addr)[0:20] → slim RLP. The
	// account structs were mutated in place by the writer (contract root/codehash
	// spliced), so EncodeAccountSlim here matches what was stored.
	for addr, acc := range accounts {
		ah := hash(addr[:])
		got := get(flat.ColAccount, flat.AccountKey(ah))
		want := nethrlp.EncodeAccountSlim(acc)
		if !bytes.Equal(got, want) {
			t.Errorf("flat Account row for %s = %x, want %x", addr.Hex(), got, want)
		}
	}

	// Storage CF: the contract slot present at the 52-byte key → RLP(trimmed).
	cah := hash(contract[:])
	skh := hash(slotKey[:])
	gotSlot := get(flat.ColStorage, flat.StorageKey(cah, skh))
	wantSlot, _ := nethrlp.EncodeStorageValue(slotVal)
	if !bytes.Equal(gotSlot, wantSlot) {
		t.Errorf("flat Storage row = %x, want %x", gotSlot, wantSlot)
	}

	// Metadata markers.
	if got := get(flat.ColMetadata, flat.LayoutKey); len(got) != 1 || got[0] != flat.LayoutFlat {
		t.Errorf("Layout marker = %x, want [%02x]", got, flat.LayoutFlat)
	}
	if got := get(flat.ColMetadata, flat.SlotEncodingKey); len(got) != 1 || got[0] != flat.SlotEncodingRLP {
		t.Errorf("SlotEncoding marker = %x, want [%02x]", got, flat.SlotEncodingRLP)
	}
	if got := get(flat.ColMetadata, flat.CurrentStateKey); !bytes.Equal(got, flat.CurrentStateValue(0, root)) {
		t.Errorf("CurrentState marker = %x, want %x", got, flat.CurrentStateValue(0, root))
	}

	// Trie nodes were persisted: the two-account state trie yields a stored root
	// node, and the contract storage trie yields a stored node.
	stateNodes := cfCount(flat.ColStateTopNodes) + cfCount(flat.ColStateNodes) + cfCount(flat.ColFallbackNodes)
	if stateNodes == 0 {
		t.Errorf("no state-trie nodes persisted across StateTopNodes/StateNodes/FallbackNodes")
	}
	if cfCount(flat.ColStorageNodes)+cfCount(flat.ColFallbackNodes) == 0 {
		t.Errorf("no storage-trie nodes persisted for the contract")
	}
}
