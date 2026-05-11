package entitygen

import (
	"bytes"
	mrand "math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

// TestCanonicalOsakaMPTRoot pins the canonical hexary-MPT state root for
// the e2e config — entitygen synthetic entities PLUS the 4 EIP system
// contracts (BeaconRoots/HistoryStorage/WithdrawalQueue/ConsolidationQueue)
// at their canonical addresses. Every MPT-mode client adapter
// (geth, nethermind, besu, reth) MUST match — same RNG draws + same
// system contracts → same state → same root, regardless of on-disk node
// layout. Drift here requires a coordinated update across all 4 client
// adapter golden tests.
//
// The hash is exported as entitygen.CanonicalOsakaMPTRoot so all 4
// client goldens + this test pin against a single source of truth.
//
// (geth-MPT in generator/generator.go uses inline RNG draws that don't
// match entitygen — known pre-existing inconsistency, tracked separately.)
func TestCanonicalOsakaMPTRoot(t *testing.T) {
	const (
		seed         = int64(12345)
		numAccounts  = 10
		numContracts = 5
		minSlots     = 1
		maxSlots     = 100
		codeSize     = 256
	)
	expected := CanonicalOsakaMPTRoot.Hex()

	rng := mrand.New(mrand.NewSource(seed))

	type acctEntry struct {
		addrHash common.Hash
		rlp      []byte
	}
	var accts []acctEntry

	for i := 0; i < numAccounts; i++ {
		acc := GenerateEOA(rng)
		buf, err := gethrlp.EncodeToBytes(acc.StateAccount)
		if err != nil {
			t.Fatalf("encode EOA %d: %v", i, err)
		}
		accts = append(accts, acctEntry{acc.AddrHash, buf})
	}

	for i := 0; i < numContracts; i++ {
		numSlots := GenerateSlotCount(rng, PowerLaw, minSlots, maxSlots)
		c := GenerateContract(rng, codeSize, numSlots)

		st := trie.NewStackTrie(nil)
		type kv struct {
			keyHash  common.Hash
			valueRLP []byte
		}
		slots := make([]kv, 0, len(c.Storage))
		for _, s := range c.Storage {
			val := s.Value
			raw := val[:]
			start := 0
			for start < len(raw) && raw[start] == 0x00 {
				start++
			}
			vrlp, err := gethrlp.EncodeToBytes(raw[start:])
			if err != nil {
				t.Fatalf("encode slot val: %v", err)
			}
			slots = append(slots, kv{
				keyHash:  crypto.Keccak256Hash(s.Key[:]),
				valueRLP: vrlp,
			})
		}
		sort.Slice(slots, func(i, j int) bool {
			return bytes.Compare(slots[i].keyHash[:], slots[j].keyHash[:]) < 0
		})
		for _, s := range slots {
			st.Update(s.keyHash[:], s.valueRLP)
		}
		c.StateAccount.Root = st.Hash()

		buf, err := gethrlp.EncodeToBytes(c.StateAccount)
		if err != nil {
			t.Fatalf("encode contract %d: %v", i, err)
		}
		accts = append(accts, acctEntry{c.AddrHash, buf})
	}

	// 4 EIP system contracts at canonical addresses — match
	// oracle.AddPragueSystemContracts (Nonce=1, Balance=0,
	// Root=EmptyRootHash, CodeHash=keccak256(code)). The 4 per-client
	// golden tests also call AddPragueSystemContracts before computing
	// state root, so this canonical hash matches what all 4 writers
	// produce for the canonical config.
	sysContracts := []struct {
		addr common.Address
		code []byte
	}{
		{params.BeaconRootsAddress, params.BeaconRootsCode},
		{params.HistoryStorageAddress, params.HistoryStorageCode},
		{params.WithdrawalQueueAddress, params.WithdrawalQueueCode},
		{params.ConsolidationQueueAddress, params.ConsolidationQueueCode},
	}
	for _, sc := range sysContracts {
		sa := &types.StateAccount{
			Nonce:    1,
			Balance:  uint256.NewInt(0),
			Root:     types.EmptyRootHash,
			CodeHash: crypto.Keccak256Hash(sc.code).Bytes(),
		}
		buf, err := gethrlp.EncodeToBytes(sa)
		if err != nil {
			t.Fatalf("encode system contract %s: %v", sc.addr.Hex(), err)
		}
		accts = append(accts, acctEntry{
			addrHash: crypto.Keccak256Hash(sc.addr[:]),
			rlp:      buf,
		})
	}

	sort.Slice(accts, func(i, j int) bool {
		return bytes.Compare(accts[i].addrHash[:], accts[j].addrHash[:]) < 0
	})

	acctTrie := trie.NewStackTrie(nil)
	for _, a := range accts {
		acctTrie.Update(a.addrHash[:], a.rlp)
	}
	got := acctTrie.Hash().Hex()
	if got != expected {
		t.Fatalf("canonical Osaka-MPT root mismatch:\n  got:  %s\n  want: %s\n  To accept this update intentionally, set CanonicalOsakaMPTRoot in canonical.go to %s and re-run the 4 client goldens.",
			got, expected, got)
	}
}
