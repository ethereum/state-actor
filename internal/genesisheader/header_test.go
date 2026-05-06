package genesisheader

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/nerolation/state-actor/genesis"
)

func TestBuild_PragueGenesis(t *testing.T) {
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 0, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	h := Build(g, 0, common.Hash{}, types.EmptyRootHash)

	if h.Number.Uint64() != 0 {
		t.Errorf("Number = %d, want 0", h.Number.Uint64())
	}
	if h.GasLimit != 30_000_000 {
		t.Errorf("GasLimit = %d, want 30_000_000", h.GasLimit)
	}
	if h.BaseFee == nil || h.BaseFee.Sign() == 0 {
		t.Error("BaseFee should be set under Prague (London active)")
	}
	if h.WithdrawalsHash == nil || *h.WithdrawalsHash != types.EmptyWithdrawalsHash {
		t.Error("WithdrawalsHash should be EmptyWithdrawalsHash under Shanghai+")
	}
	if h.ParentBeaconRoot == nil || h.ExcessBlobGas == nil || h.BlobGasUsed == nil {
		t.Error("Cancun fields should all be set (zero-valued) under Cancun+")
	}
	if h.RequestsHash == nil || *h.RequestsHash != types.EmptyRequestsHash {
		t.Error("RequestsHash should be EmptyRequestsHash under Prague")
	}
	// Canonical empties
	if h.UncleHash != types.EmptyUncleHash {
		t.Error("UncleHash should be canonical empty")
	}
	if h.TxHash != types.EmptyTxsHash {
		t.Error("TxHash should be canonical empty")
	}
	if h.ReceiptHash != types.EmptyRootHash {
		t.Error("ReceiptHash should be EmptyRootHash for empty receipts")
	}
	if h.Extra == nil {
		t.Error("Extra should be []byte{} not nil")
	}
}

func TestBuild_ShanghaiNoCancunFields(t *testing.T) {
	g, err := genesis.BuildSynthetic("shanghai", big.NewInt(1), 0, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	h := Build(g, 0, common.Hash{}, types.EmptyRootHash)

	if h.WithdrawalsHash == nil {
		t.Error("WithdrawalsHash should be set under Shanghai")
	}
	if h.ParentBeaconRoot != nil {
		t.Error("ParentBeaconRoot should NOT be set under Shanghai (Cancun-only)")
	}
	if h.RequestsHash != nil {
		t.Error("RequestsHash should NOT be set under Shanghai (Prague-only)")
	}
}

func TestBuild_PreLondonNoBaseFee(t *testing.T) {
	g, err := genesis.BuildSynthetic("istanbul", big.NewInt(1), 0, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	h := Build(g, 0, common.Hash{}, types.EmptyRootHash)

	if h.BaseFee != nil {
		t.Error("BaseFee should be nil pre-London")
	}
}

func TestBuild_HeaderHashStable(t *testing.T) {
	// Same inputs → same hash. Catches accidental nondeterminism.
	g, _ := genesis.BuildSynthetic("prague", big.NewInt(1337), 25_000_000, 100, []byte{0x01, 0x02})
	h1 := Build(g, 0, common.Hash{}, types.EmptyRootHash)
	h2 := Build(g, 0, common.Hash{}, types.EmptyRootHash)
	if h1.Hash() != h2.Hash() {
		t.Errorf("non-deterministic header hash: %s vs %s", h1.Hash().Hex(), h2.Hash().Hex())
	}
	if h1.GasLimit != 25_000_000 {
		t.Errorf("GasLimit = %d, want 25M (from BuildSynthetic)", h1.GasLimit)
	}
	if h1.Time != 100 {
		t.Errorf("Time = %d, want 100 (from BuildSynthetic)", h1.Time)
	}
}

func TestBuild_GasLimitFallbackOnZero(t *testing.T) {
	// Genesis with gasLimit=0 should fall back to params.GenesisGasLimit.
	g := &genesis.Genesis{
		Config:   &params.ChainConfig{ChainID: big.NewInt(1)},
		GasLimit: 0,
	}
	h := Build(g, 0, common.Hash{}, types.EmptyRootHash)
	if h.GasLimit != params.GenesisGasLimit {
		t.Errorf("GasLimit fallback = %d, want params.GenesisGasLimit (%d)", h.GasLimit, params.GenesisGasLimit)
	}
}
