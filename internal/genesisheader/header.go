// Package genesisheader provides a shared genesis-block header builder.
//
// Before this package, geth (client/geth/genesis_block.go) and reth
// (client/reth/header.go) each carried a near-identical fork ladder
// (London/Shanghai/Cancun/Prague) that read the same fields off
// *genesis.Genesis and produced a *types.Header. ~50 lines duplicated.
//
// nethermind and besu carried subsets (no Cancun+); their writers will
// migrate here once they grow post-Merge support (see
// genesis.MaxForkForClient).
package genesisheader

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/nerolation/state-actor/genesis"
)

// Build constructs a *types.Header for the genesis block described by g
// at block `number`, with the supplied parentHash and stateRoot. Empty-
// body fields are set to their canonical values (matching reth's
// expectation; geth's types.NewBlock would overwrite them with the same
// values when constructing a Block, so this is a safe normalization for
// both clients).
//
// Fork-conditional fields:
//   - London: BaseFee (from g.BaseFee or params.InitialBaseFee fallback)
//   - Shanghai: WithdrawalsHash = types.EmptyWithdrawalsHash
//   - Cancun: ParentBeaconRoot, ExcessBlobGas, BlobGasUsed
//   - Prague: RequestsHash = types.EmptyRequestsHash
//
// g.Config must be non-nil and have ChainID set; the per-fork predicates
// dispatch on it.
func Build(g *genesis.Genesis, number uint64, parentHash, stateRoot common.Hash) *types.Header {
	if g == nil || g.Config == nil {
		// Defensive — callers always pass a non-nil *Genesis built by
		// genesis.BuildSynthetic. If we hit this, the caller is buggy.
		panic("genesisheader.Build: nil genesis or genesis.Config")
	}

	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = params.GenesisGasLimit
	}
	difficulty := (*big.Int)(g.Difficulty)
	if difficulty == nil {
		difficulty = big.NewInt(0)
	}
	extra := []byte(g.ExtraData)
	if extra == nil {
		extra = []byte{}
	}

	header := &types.Header{
		Number:      new(big.Int).SetUint64(number),
		Nonce:       types.EncodeNonce(uint64(g.Nonce)),
		Time:        uint64(g.Timestamp),
		ParentHash:  parentHash,
		Extra:       extra,
		GasLimit:    gasLimit,
		GasUsed:     uint64(g.GasUsed),
		Difficulty:  difficulty,
		MixDigest:   g.Mixhash,
		Coinbase:    g.Coinbase,
		Root:        stateRoot,
		UncleHash:   types.EmptyUncleHash,
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyRootHash,
	}

	num := new(big.Int).SetUint64(number)
	ts := uint64(g.Timestamp)

	if g.Config.IsLondon(num) {
		if g.BaseFee != nil {
			header.BaseFee = (*big.Int)(g.BaseFee)
		} else {
			header.BaseFee = big.NewInt(params.InitialBaseFee)
		}
	}
	if g.Config.IsShanghai(num, ts) {
		emptyWithdrawalsHash := types.EmptyWithdrawalsHash
		header.WithdrawalsHash = &emptyWithdrawalsHash
	}
	if g.Config.IsCancun(num, ts) {
		header.ParentBeaconRoot = new(common.Hash)
		if g.ExcessBlobGas != nil {
			excess := uint64(*g.ExcessBlobGas)
			header.ExcessBlobGas = &excess
		} else {
			header.ExcessBlobGas = new(uint64)
		}
		if g.BlobGasUsed != nil {
			used := uint64(*g.BlobGasUsed)
			header.BlobGasUsed = &used
		} else {
			header.BlobGasUsed = new(uint64)
		}
	}
	if g.Config.IsPrague(num, ts) {
		emptyRequestsHash := types.EmptyRequestsHash
		header.RequestsHash = &emptyRequestsHash
	}
	return header
}
