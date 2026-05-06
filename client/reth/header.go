package reth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/genesisheader"
)

// buildBlock0Header builds the canonical genesis block header for the cgo
// direct-write path. Number=0; ParentHash=zero; Root must be the actual
// state root computed from the generated entities (or types.EmptyRootHash
// for empty alloc). The header.Hash() must match the chainspec-derived
// genesis hash that reth boot validates.
//
// Thin wrapper around internal/genesisheader.Build — kept so call sites
// in run_cgo.go don't need to know about the shared package and can
// continue to read like a per-client builder.
func buildBlock0Header(g *genesis.Genesis) (*types.Header, error) {
	return genesisheader.Build(g, 0, common.Hash{}, types.EmptyRootHash), nil
}
