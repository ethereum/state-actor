//go:build cgo_erigon_commitment

package commitment

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// goldenBFixtures are the adversarial allocs Golden B runs three-way over.
func goldenBFixtures(t *testing.T) map[string][]Account {
	t.Helper()
	single := []Account{{
		Address: common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Nonce:   7, Balance: uint256.NewInt(1e15),
	}}
	oneSlot := []Account{{
		Address: common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Nonce:   1, Balance: uint256.NewInt(42),
		Storage: map[common.Hash]common.Hash{
			common.HexToHash("0x01"): common.HexToHash("0xbeef"),
		},
	}}
	// 64 slots force a multi-level storage subtree under the account leaf.
	multiSlot := []Account{{
		Address: common.HexToAddress("0x3333333333333333333333333333333333333333"),
		Nonce:   9, Balance: uint256.NewInt(1),
		Storage: func() map[common.Hash]common.Hash {
			m := map[common.Hash]common.Hash{}
			for j := 0; j < 64; j++ {
				var k, v common.Hash
				k[31], v[31] = byte(j+1), byte(j+101)
				m[k] = v
			}
			return m
		}(),
	}}
	// Code-bearing shapes: the CodeUpdate flag changes the account leaf hash,
	// so byte-identity must cover it (real allocs are ~30% delegated EOAs).
	delegated := []Account{{
		Address: common.HexToAddress("0x4444444444444444444444444444444444444444"),
		Nonce:   1, Balance: uint256.NewInt(5),
		Code: append([]byte{0xef, 0x01, 0x00}, common.HexToAddress("0x5555555555555555555555555555555555555555").Bytes()...),
	}}
	codeAndStorage := []Account{{
		Address: common.HexToAddress("0x6666666666666666666666666666666666666666"),
		Nonce:   3, Balance: uint256.NewInt(7),
		Code: []byte{0x60, 0x80, 0x60, 0x40, 0x52, 0x00},
		Storage: map[common.Hash]common.Hash{
			common.HexToHash("0x02"): common.HexToHash("0xcafe"),
			common.HexToHash("0x03"): common.HexToHash("0xf00d"),
		},
	}}
	// oneNibble: 24 accounts whose keccak(addr) all share first nibble 0x7 —
	// one hot shard, 15 empty shards (foldNibble skipped for empties;
	// propagate chains at the root row).
	oneNibble := make([]Account, 0, 24)
	for i := uint64(1); len(oneNibble) < 24; i++ {
		var addr common.Address
		binary.BigEndian.PutUint64(addr[12:], i)
		if crypto.Keccak256(addr[:])[0]>>4 == 0x7 {
			oneNibble = append(oneNibble, Account{Address: addr, Nonce: i, Balance: uint256.NewInt(i * 3)})
		}
	}
	return map[string][]Account{
		"span2048":       keccakSpanFixture(2048),
		"single":         single,
		"oneSlot":        oneSlot,
		"multiSlot":      multiSlot,
		"oneNibble":      oneNibble,
		"delegated":      delegated,
		"codeAndStorage": codeAndStorage,
		"empty":          {},
	}
}

// TestGoldenB_DirectDriveMatchesEngine is the DDF byte-identity gate: for
// each fixture, three builds must agree on root, BranchCount, and every
// branch row byte —
//
//	engine/plain  : the Updates/etl engine over plain-keyed stores
//	engine/hashed : the Updates/etl engine over hashed-keyed stores
//	DIRECT/hashed : the Direct-Drive Fold over hashed-keyed stores
func TestGoldenB_DirectDriveMatchesEngine(t *testing.T) {
	restoreChunk := setCommitmentChunkKeys(0) // hashed keying requires single-shot
	defer restoreChunk()

	for name, accounts := range goldenBFixtures(t) {
		t.Run(name, func(t *testing.T) {
			restore := setDirectEnabled(false)
			enginePlain, err := ComputeGenesisRootFromAccountsKeyed(accounts, KeyingPlain)
			if err != nil {
				t.Fatalf("engine/plain: %v", err)
			}
			engineHashed, err := ComputeGenesisRootFromAccountsKeyed(accounts, KeyingHashed)
			if err != nil {
				t.Fatalf("engine/hashed: %v", err)
			}
			restore()

			restore = setDirectEnabled(true)
			direct, err := ComputeGenesisRootFromAccountsKeyed(accounts, KeyingHashed)
			if err != nil {
				t.Fatalf("DIRECT/hashed: %v", err)
			}
			restore()

			for _, pair := range []struct {
				label string
				got   Result
			}{{"engine/hashed", engineHashed}, {"DIRECT/hashed", direct}} {
				if pair.got.Root != enginePlain.Root {
					t.Fatalf("%s ROOT DIVERGED: %s vs engine/plain %s",
						pair.label, pair.got.Root.Hex(), enginePlain.Root.Hex())
				}
				if pair.got.BranchCount != enginePlain.BranchCount {
					t.Errorf("%s BranchCount = %d, engine/plain = %d",
						pair.label, pair.got.BranchCount, enginePlain.BranchCount)
				}
				if !bytes.Equal(branchNodesBytes(pair.got.BranchNodes), branchNodesBytes(enginePlain.BranchNodes)) {
					t.Errorf("%s branch BYTES diverged from engine/plain", pair.label)
				}
				if !bytes.Equal(pair.got.HPHState, enginePlain.HPHState) {
					t.Errorf("%s HPHState diverged from engine/plain", pair.label)
				}
			}
			t.Logf("%s: root %s, %d branch rows — engine/plain == engine/hashed == DIRECT",
				name, enginePlain.Root.Hex(), enginePlain.BranchCount)
		})
	}
}
