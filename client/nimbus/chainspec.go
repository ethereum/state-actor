package nimbus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ethereum/state-actor/genesis"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

// GenesisFileName is the sidecar written at the datadir root for
// `nimbus executionClient --network=<datadir>/nimbus-genesis.json`.
const GenesisFileName = nimbusinternal.GenesisFileName

// StoreDir is where the RocksDB lives: nimbus always opens <data-dir>/ecdb
// (core_db/backend/rocksdb_desc.nim DbFolder); there is no flag for it.
func StoreDir(datadir string) string {
	return filepath.Join(datadir, nimbusinternal.DBFolder)
}

// WriteGenesisSidecar writes <datadir>/nimbus-genesis.json.
func WriteGenesisSidecar(datadir string, g *genesis.Genesis) error {
	payload, err := BuildGenesisJSON(g)
	if err != nil {
		return fmt.Errorf("nimbus: build genesis sidecar: %w", err)
	}
	outPath := filepath.Join(datadir, GenesisFileName)
	if err := os.WriteFile(outPath, payload, 0o644); err != nil {
		return fmt.Errorf("nimbus: write genesis sidecar: %w", err)
	}
	return nil
}

// defaultDepositContractAddress is the mainnet beacon deposit contract, used
// when the genesis config leaves depositContractAddress unset (nimbus would
// otherwise default it to the zero address).
const defaultDepositContractAddress = "0x00000000219ab540356cbb839cbe05303d7705fa"

// BuildGenesisJSON renders the Geth-style genesis file nimbus's
// chain_config_loader.nim parses. Field-name matching there is exact and
// case-sensitive and unknown fields are ignored, so the shape below sticks
// to what the loader reads:
//
//   - fork times are JSON NUMBERS (Opt[EthTime] is read with parseInt) and
//     are emitted only when the fork is scheduled;
//   - the genesis timestamp is a JSON STRING (hex or decimal);
//   - mixHash must be spelled in camelCase (a lowercase key is dropped);
//   - blobSchedule entries are inherited by later forks that lack one, so
//     cancun and prague are always present and osaka follows osakaTime.
//
// alloc is intentionally empty: the account and storage tries are written
// directly into AriVtx and the real state root is in the stored genesis
// header. Nimbus normally rebuilds genesis from this alloc on every start
// and refuses a datadir whose block-0 hash disagrees; the boot recipe passes
// --debug-rewrite-datadir-id to skip that check (see doc.go).
func BuildGenesisJSON(g *genesis.Genesis) ([]byte, error) {
	if g == nil || g.Config == nil {
		return nil, fmt.Errorf("BuildGenesisJSON: nil genesis or genesis.Config")
	}
	chainID := int64(1337)
	if g.Config.ChainID != nil {
		chainID = g.Config.ChainID.Int64()
	}
	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}

	cfg := map[string]any{
		"chainId":                 chainID,
		"homesteadBlock":          0,
		"eip150Block":             0,
		"eip155Block":             0,
		"eip158Block":             0,
		"byzantiumBlock":          0,
		"constantinopleBlock":     0,
		"petersburgBlock":         0,
		"istanbulBlock":           0,
		"berlinBlock":             0,
		"londonBlock":             0,
		"mergeNetsplitBlock":      0,
		"terminalTotalDifficulty": "0x0",
	}
	blobSchedule := map[string]any{
		"cancun": map[string]any{"target": 3, "max": 6, "baseFeeUpdateFraction": 3338477},
		"prague": map[string]any{"target": 6, "max": 9, "baseFeeUpdateFraction": 5007716},
	}
	if g.Config.ShanghaiTime != nil {
		cfg["shanghaiTime"] = *g.Config.ShanghaiTime
	}
	if g.Config.CancunTime != nil {
		cfg["cancunTime"] = *g.Config.CancunTime
	}
	if g.Config.PragueTime != nil {
		cfg["pragueTime"] = *g.Config.PragueTime
	}
	if g.Config.OsakaTime != nil {
		cfg["osakaTime"] = *g.Config.OsakaTime
		blobSchedule["osaka"] = map[string]any{"target": 6, "max": 9, "baseFeeUpdateFraction": 5007716}
	}
	cfg["blobSchedule"] = blobSchedule

	depositAddr := g.Config.DepositContractAddress
	if depositAddr == (common.Address{}) {
		depositAddr = common.HexToAddress(defaultDepositContractAddress)
	}
	cfg["depositContractAddress"] = depositAddr.Hex()

	difficulty := "0x0"
	if g.Difficulty != nil {
		difficulty = fmt.Sprintf("0x%x", g.Difficulty.ToInt())
	}
	doc := map[string]any{
		"config":     cfg,
		"nonce":      fmt.Sprintf("0x%x", uint64(g.Nonce)),
		"timestamp":  fmt.Sprintf("0x%x", uint64(g.Timestamp)),
		"extraData":  hexutil.Encode(g.ExtraData),
		"gasLimit":   fmt.Sprintf("0x%x", gasLimit),
		"difficulty": difficulty,
		"mixHash":    g.Mixhash.Hex(),
		"coinbase":   g.Coinbase.Hex(),
		"alloc":      map[string]any{},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("BuildGenesisJSON marshal: %w", err)
	}
	return append(out, '\n'), nil
}
