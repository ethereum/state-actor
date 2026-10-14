package generator

import (
	"iter"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/templates"
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

// TestPreAllocLazyCode verifies Validate() routes LazyCode to
// GenesisLazyCode (not GenesisCode), and CodeAt / CodeSizeAt read both.
func TestPreAllocLazyCode(t *testing.T) {
	eager := common.HexToAddress("0x000000000000000000000000000000000000cc01")
	lazy := common.HexToAddress("0x000000000000000000000000000000000000cc02")
	none := common.HexToAddress("0x000000000000000000000000000000000000cc03")
	lazyBytes := []byte{0x5b, 0x5b, 0x00}
	calls := 0
	acct := func() *types.StateAccount {
		return &types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0), Root: types.EmptyRootHash}
	}

	cfg := &Config{
		PreAlloc: []templates.PreAllocEntity{
			{Address: eager, Account: acct(), Code: []byte{0x60, 0x80}},
			{Address: lazy, Account: acct(), LazyCode: &templates.LazyCode{
				Size: len(lazyBytes),
				Make: func() []byte { calls++; return append([]byte(nil), lazyBytes...) },
			}},
			{Address: none, Account: acct()},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if calls != 0 {
		t.Errorf("Validate must not make lazy code; Make called %d times", calls)
	}
	if _, ok := cfg.GenesisCode[lazy]; ok {
		t.Error("lazy code must not be copied into GenesisCode")
	}
	if cfg.GenesisLazyCode[lazy] == nil {
		t.Fatal("lazy code missing from GenesisLazyCode")
	}

	cases := []struct {
		addr common.Address
		want []byte
	}{
		{eager, []byte{0x60, 0x80}},
		{lazy, lazyBytes},
		{none, nil},
	}
	for _, c := range cases {
		if got := cfg.CodeSizeAt(c.addr); got != len(c.want) {
			t.Errorf("CodeSizeAt(%s) = %d, want %d", c.addr.Hex(), got, len(c.want))
		}
		if got := cfg.CodeAt(c.addr); string(got) != string(c.want) {
			t.Errorf("CodeAt(%s) = %x, want %x", c.addr.Hex(), got, c.want)
		}
	}
	if calls != 1 {
		t.Errorf("CodeSizeAt must not make lazy code; Make called %d times, want 1", calls)
	}
}

// TestValidateRejectsCodeInBothMaps verifies an address cannot have both
// eager and lazy code.
func TestValidateRejectsCodeInBothMaps(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000cc04")
	cfg := &Config{
		GenesisAccounts: map[common.Address]*types.StateAccount{
			addr: {Nonce: 1, Balance: uint256.NewInt(0), Root: types.EmptyRootHash},
		},
		GenesisCode: map[common.Address][]byte{addr: {0x00}},
		GenesisLazyCode: map[common.Address]*templates.LazyCode{
			addr: {Size: 1, Make: func() []byte { return []byte{0x00} }},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must reject an address in both GenesisCode and GenesisLazyCode")
	}
}
