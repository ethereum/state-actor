// Package genesis handles reading, parsing, and converting genesis JSON
// configurations into client-neutral Go types.
//
// The package is deliberately client-neutral: it parses the genesis.json
// format used by ethereum-package and devnets and exposes the alloc as
// Go data structures (types.StateAccount maps, raw storage maps, raw code
// maps). It does NOT write genesis blocks to any client database — that
// responsibility lives in client/<name>/ packages, each producing the on-disk
// shape its target client expects (e.g. client/geth.WriteGenesisBlock for
// geth's Pebble + freezer layout).
package genesis

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// Genesis represents the genesis block configuration.
// This is a simplified version of go-ethereum's Genesis struct
// that handles the JSON format used by ethereum-package and devnets.
type Genesis struct {
	Config     *params.ChainConfig `json:"config"`
	Nonce      hexutil.Uint64      `json:"nonce"`
	Timestamp  hexutil.Uint64      `json:"timestamp"`
	ExtraData  hexutil.Bytes       `json:"extraData"`
	GasLimit   hexutil.Uint64      `json:"gasLimit"`
	Difficulty *hexutil.Big        `json:"difficulty"`
	Mixhash    common.Hash         `json:"mixHash"`
	Coinbase   common.Address      `json:"coinbase"`
	Alloc      GenesisAlloc        `json:"alloc"`

	// Block fields (optional in genesis.json)
	Number        hexutil.Uint64  `json:"number"`
	GasUsed       hexutil.Uint64  `json:"gasUsed"`
	ParentHash    common.Hash     `json:"parentHash"`
	BaseFee       *hexutil.Big    `json:"baseFeePerGas"`
	ExcessBlobGas *hexutil.Uint64 `json:"excessBlobGas"`
	BlobGasUsed   *hexutil.Uint64 `json:"blobGasUsed"`
}

// GenesisAlloc is the genesis allocation map.
type GenesisAlloc map[common.Address]GenesisAccount

// GenesisAccount represents an account in the genesis allocation.
type GenesisAccount struct {
	Code    hexutil.Bytes               `json:"code,omitempty"`
	Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
	Balance *hexutil.Big                `json:"balance"`
	Nonce   hexutil.Uint64              `json:"nonce,omitempty"`
}

// LoadGenesis loads a genesis configuration from a JSON file. Used by
// tests; the production CLI no longer accepts --genesis (state-actor
// builds genesis itself via BuildSynthetic).
func LoadGenesis(path string) (*Genesis, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read genesis file: %w", err)
	}

	var genesis Genesis
	if err := json.Unmarshal(data, &genesis); err != nil {
		return nil, fmt.Errorf("failed to parse genesis JSON: %w", err)
	}

	return &genesis, nil
}

// OrDefault returns g if non-nil, otherwise builds a default *Genesis with
// DefaultFork active and chainID 1337. Tests that exercise client Run
// paths use this so they don't need to call BuildSynthetic explicitly;
// production callers always set Config.Genesis via main.go and never see
// the default branch.
func OrDefault(g *Genesis) *Genesis {
	if g != nil {
		return g
	}
	out, _ := BuildSynthetic("", nil, 0, 0, nil)
	return out
}

// BuildSynthetic constructs an in-memory *Genesis with the named fork
// active at genesis (block 0 / time 0) and the supplied header knobs.
// Replaces the LoadGenesis-from-disk path: state-actor's CLI no longer
// accepts --genesis; the four header fields users actually need to vary
// (chainID, gasLimit, timestamp, extraData) are exposed as flags.
//
// Defaults applied when fields are zero:
//   - fork == "" → DefaultFork ("prague")
//   - chainID == nil → 1337 (devnet convention)
//   - gasLimit == 0 → 30_000_000
//   - timestamp == 0 → 0 (genesis epoch)
//   - extraData == nil → empty slice
//
// Difficulty is fixed at 0 (post-Merge); BaseFee is set to
// params.InitialBaseFee when London is active in the resolved
// ChainConfig (so London+ headers are structurally complete). Alloc is
// always empty — pre-funded accounts come from --inject-accounts.
func BuildSynthetic(fork string, chainID *big.Int, gasLimit uint64, timestamp uint64, extraData []byte) (*Genesis, error) {
	if chainID == nil {
		chainID = big.NewInt(1337)
	}
	cfg, err := BuildChainConfigForFork(fork, chainID)
	if err != nil {
		return nil, err
	}
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}
	if extraData == nil {
		extraData = []byte{}
	}
	g := &Genesis{
		Config:     cfg,
		Nonce:      0,
		Timestamp:  hexutil.Uint64(timestamp),
		ExtraData:  hexutil.Bytes(extraData),
		GasLimit:   hexutil.Uint64(gasLimit),
		Difficulty: (*hexutil.Big)(big.NewInt(0)),
		Alloc:      GenesisAlloc{},
	}
	if cfg.IsLondon(big.NewInt(0)) {
		g.BaseFee = (*hexutil.Big)(big.NewInt(params.InitialBaseFee))
	}
	return g, nil
}

// ToStateAccounts converts the genesis alloc to types.StateAccount format
// suitable for state generation.
func (g *Genesis) ToStateAccounts() map[common.Address]*types.StateAccount {
	accounts := make(map[common.Address]*types.StateAccount, len(g.Alloc))

	for addr, alloc := range g.Alloc {
		var balance *uint256.Int
		if alloc.Balance != nil {
			balance, _ = uint256.FromBig((*big.Int)(alloc.Balance))
		}
		if balance == nil {
			balance = new(uint256.Int)
		}

		// Compute code hash
		codeHash := types.EmptyCodeHash
		if len(alloc.Code) > 0 {
			codeHash = crypto.Keccak256Hash(alloc.Code)
		}

		accounts[addr] = &types.StateAccount{
			Nonce:    uint64(alloc.Nonce),
			Balance:  balance,
			Root:     types.EmptyRootHash, // Will be updated if storage exists
			CodeHash: codeHash.Bytes(),
		}
	}

	return accounts
}

// GetAllocStorage returns the storage for genesis alloc accounts.
func (g *Genesis) GetAllocStorage() map[common.Address]map[common.Hash]common.Hash {
	storage := make(map[common.Address]map[common.Hash]common.Hash)

	for addr, alloc := range g.Alloc {
		if len(alloc.Storage) > 0 {
			storage[addr] = alloc.Storage
		}
	}

	return storage
}

// GetAllocCode returns the code for genesis alloc accounts.
func (g *Genesis) GetAllocCode() map[common.Address][]byte {
	code := make(map[common.Address][]byte)

	for addr, alloc := range g.Alloc {
		if len(alloc.Code) > 0 {
			code[addr] = alloc.Code
		}
	}

	return code
}
