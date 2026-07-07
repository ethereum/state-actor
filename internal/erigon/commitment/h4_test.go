//go:build cgo_erigon_commitment

package commitment

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// TestH4_HexPatriciaHashed_MatchesMPT pins the empirical resolution
// to Hypothesis H4: Erigon's HexPatriciaHashed produces the SAME
// state root as geth's standard MPT for an identical alloc.
//
// Initial test runs (2026-05-26) showed divergence (geth 0x639b…
// vs Erigon 0x020b…), which was diagnosed and traced via a 3-Opus-
// agent investigation to a byte-alignment bug in this test's own
// Erigon-side input construction. Specifically, the storage Update's
// Storage field was being populated right-aligned via erigonHash(value)
// while Erigon's TouchStorage invariant (commitment.go:1746-1753)
// requires LEFT alignment at Storage[0:StorageLen]. The fix in
// commitment.go now copies trimmed bytes to Storage[0] and the
// algorithms produce byte-identical roots.
//
// Tests run with `go test -tags "cgo_erigon_commitment gofuzz"`.
func TestH4_HexPatriciaHashed_MatchesMPT(t *testing.T) {
	addrEOA1 := common.HexToAddress("0x000000000000000000000000000000000000beef")
	addrEOA2 := common.HexToAddress("0x000000000000000000000000000000000000cafe")
	addrEOA3 := common.HexToAddress("0x00000000000000000000000000000000deadbeef")
	addrEOA4 := common.HexToAddress("0xff00000000000000000000000000000000001234")
	addrEOA5 := common.HexToAddress("0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")

	addrContract := common.HexToAddress("0x0000000000000000000000000000000000c0ffee")
	contractCode := []byte{0x60, 0x80, 0x60, 0x40, 0x52, 0x60, 0x05, 0x60, 0x00, 0x55}
	contractStorage := map[common.Hash]common.Hash{
		common.BigToHash(big.NewInt(0)):      common.BigToHash(big.NewInt(100)),
		common.BigToHash(big.NewInt(1)):      common.BigToHash(big.NewInt(200)),
		common.BigToHash(big.NewInt(0x1000)): common.BigToHash(big.NewInt(0xdeadbeef)),
	}

	// Build alloc for both computations from the same source data.
	// Both code paths consume the SAME (address, balance, nonce, code,
	// storage) tuples.
	src := []srcAccount{
		{addrEOA1, 0, big.NewInt(1_000_000_000_000_000_000), nil, nil},
		{addrEOA2, 5, big.NewInt(0), nil, nil},
		{addrEOA3, 100, big.NewInt(42), nil, nil},
		{addrEOA4, 0, big.NewInt(0), nil, nil},
		{addrEOA5, 1, big.NewInt(0).Mul(big.NewInt(999_999_999), big.NewInt(1_000_000_000_000_000_000)), nil, nil},
		{addrContract, 1, big.NewInt(42), contractCode, contractStorage},
	}

	// Compute root via geth's MPT (canonical genesis encoding):
	// stateTrie["addrHash"] = rlp(StateAccount{Nonce, Balance, StorageRoot, CodeHash}).
	gethRoot := computeMPTGenesisRoot(t, src)

	// Compute root via state-actor's commitment writer (vendored Erigon
	// HexPatriciaHashed).
	erigonAccts := make([]Account, len(src))
	for i, s := range src {
		bal := uint256.NewInt(0)
		if s.Balance != nil {
			bal, _ = uint256.FromBig(s.Balance)
		}
		erigonAccts[i] = Account{
			Address: s.Addr,
			Nonce:   s.Nonce,
			Balance: bal,
			Code:    s.Code,
			Storage: s.Storage,
		}
	}
	res, err := ComputeGenesisRootFromAccounts(erigonAccts)
	if err != nil {
		t.Fatalf("ComputeGenesisRoot: %v", err)
	}
	defer res.CloseBranches()

	t.Logf("geth   MPT root:          %s", gethRoot.Hex())
	t.Logf("erigon HexPatriciaHashed: %s", res.Root.Hex())

	if gethRoot != res.Root {
		t.Fatalf("MPT ≢ HexPatriciaHashed on identical alloc — the algorithms diverged again:\n  geth MPT: %s\n  Erigon HPH: %s\nThis is a regression. Re-run the 3-Opus-agent root-cause investigation documented in the plan.",
			gethRoot.Hex(), res.Root.Hex())
	}
	t.Logf("MPT ≡ HexPatriciaHashed confirmed on this fixture.")
}

// srcAccount is the shared test-input shape consumed by both the
// MPT and HexPatriciaHashed paths.
type srcAccount struct {
	Addr    common.Address
	Nonce   uint64
	Balance *big.Int
	Code    []byte
	Storage map[common.Hash]common.Hash
}

// computeMPTGenesisRoot mirrors what geth's core.Genesis.ToBlock()
// does for the genesis state: build a state trie, insert each
// account's RLP-encoded StateAccount, return the root.
func computeMPTGenesisRoot(t *testing.T, src []srcAccount) common.Hash {
	t.Helper()
	stateTrie := trie.NewEmpty(triedb.NewDatabase(nil, nil))

	for _, s := range src {
		// Compute per-account StorageRoot if storage is present.
		storageRoot := types.EmptyRootHash
		if len(s.Storage) > 0 {
			storageTrie := trie.NewEmpty(triedb.NewDatabase(nil, nil))
			for slot, value := range s.Storage {
				slotHash := common.BytesToHash(keccak(slot[:]))
				valRLP, err := rlp.EncodeToBytes(value[:])
				if err != nil {
					t.Fatalf("rlp encode storage value: %v", err)
				}
				// Trim leading zeros (canonical storage encoding).
				trimmed := trimLeadingZerosTest(value[:])
				if len(trimmed) > 0 {
					trimmedRLP, err := rlp.EncodeToBytes(trimmed)
					if err != nil {
						t.Fatalf("rlp encode trimmed storage value: %v", err)
					}
					valRLP = trimmedRLP
				}
				storageTrie.MustUpdate(slotHash.Bytes(), valRLP)
			}
			storageRoot = storageTrie.Hash()
		}

		// Build StateAccount.
		balU256 := uint256.NewInt(0)
		if s.Balance != nil {
			balU256, _ = uint256.FromBig(s.Balance)
		}
		codeHash := types.EmptyCodeHash
		if len(s.Code) > 0 {
			codeHash = common.BytesToHash(keccak(s.Code))
		}
		stateAcct := &types.StateAccount{
			Nonce:    s.Nonce,
			Balance:  balU256,
			Root:     storageRoot,
			CodeHash: codeHash.Bytes(),
		}
		acctRLP, err := rlp.EncodeToBytes(stateAcct)
		if err != nil {
			t.Fatalf("rlp encode account: %v", err)
		}
		addrHash := common.BytesToHash(keccak(s.Addr[:]))
		stateTrie.MustUpdate(addrHash.Bytes(), acctRLP)
	}
	return stateTrie.Hash()
}

func keccak(b []byte) []byte {
	h := newKeccak()
	_, _ = h.Write(b)
	out := make([]byte, 32)
	copy(out, h.Sum(nil))
	return out
}

func trimLeadingZerosTest(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}
