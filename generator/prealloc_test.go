package generator

import (
	"iter"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/templates"
)

// TestPreAllocShimMaterializes verifies Validate() folds PreAlloc
// account headers + code into GenesisAccounts/Code. Storage is NOT
// drained — it stays as iter.Seq2 on c.PreAlloc for streaming consumers.
func TestPreAllocShimMaterializes(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000aaaa")

	cfg := &Config{
		PreAlloc: []templates.PreAllocEntity{{
			Address: addr,
			Account: &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(1000),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash[:],
			},
			Code: []byte{0x60, 0x80},
			Storage: storageMap(map[common.Hash]common.Hash{
				common.HexToHash("0x01"): common.HexToHash("0xaa"),
				common.HexToHash("0x02"): common.HexToHash("0xbb"),
			}),
		}},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if got := cfg.GenesisAccounts[addr]; got == nil {
		t.Fatal("shim did not materialize account into GenesisAccounts")
	}
	if got := cfg.GenesisCode[addr]; len(got) != 2 {
		t.Errorf("shim did not materialize code: got %v", got)
	}
	if got := cfg.GenesisStorage[addr]; len(got) != 0 {
		t.Errorf("shim should NOT drain storage; got %v entries", len(got))
	}
	if len(cfg.PreAlloc) != 1 {
		t.Errorf("Validate should preserve PreAlloc; got %d entries", len(cfg.PreAlloc))
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("second Validate must succeed (idempotent): %v", err)
	}
}

func TestPreAllocShimRejectsCollisionWithGenesisAccounts(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000bbbb")
	cfg := &Config{
		GenesisAccounts: map[common.Address]*types.StateAccount{
			addr: {Nonce: 0, Balance: uint256.NewInt(0), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash[:]},
		},
		PreAlloc: []templates.PreAllocEntity{{
			Address: addr,
			Account: &types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash[:]},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected collision error between programmatic alloc + spec alloc")
	}
}

// TestValidateAcceptsSpecRegardlessOfStorageSize confirms Validate
// doesn't reject specs based on slot count — target-size enforcement
// lives in each writer's dirSize sampling.
func TestValidateAcceptsSpecRegardlessOfStorageSize(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000dddd")
	bigStorage := make(map[common.Hash]common.Hash, 1000)
	for i := 0; i < 1000; i++ {
		var k common.Hash
		k[31] = byte(i)
		bigStorage[k] = common.HexToHash("0xaa")
	}

	cfg := &Config{
		TargetSize: 1000,
		PreAlloc: []templates.PreAllocEntity{{
			Address: addr,
			Account: &types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash[:]},
			Storage: storageMap(bigStorage),
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate should not reject on storage size: %v", err)
	}
}

func TestValidateAcceptsSpecUnderTargetSize(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000eeee")
	smallStorage := map[common.Hash]common.Hash{
		common.HexToHash("0x01"): common.HexToHash("0xaa"),
	}
	cfg := &Config{
		TargetSize: 1_000_000,
		PreAlloc: []templates.PreAllocEntity{{
			Address: addr,
			Account: &types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash[:]},
			Storage: storageMap(smallStorage),
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected pass under budget, got %v", err)
	}
}

// TestPreAllocShimEmpty asserts an empty Config (no AutoFill / PreAlloc /
// GenesisAccounts) is rejected by Validate — the writer needs at least
// one source of entities to emit.
func TestPreAllocShimEmpty(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate should reject empty Config (no AutoFill/PreAlloc/GenesisAccounts)")
	}
}

func storageMap(m map[common.Hash]common.Hash) iter.Seq2[common.Hash, common.Hash] {
	if len(m) == 0 {
		return nil
	}
	return func(yield func(common.Hash, common.Hash) bool) {
		for k, v := range m {
			if !yield(k, v) {
				return
			}
		}
	}
}
