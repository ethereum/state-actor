package ethrex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nerolation/state-actor/genesis"
	ethrexinternal "github.com/nerolation/state-actor/internal/ethrex"
)

// StoreDir returns the directory ethrex opens its RocksDB from for a custom
// --network genesis: <datadir>/chain-<chainid> (ethrex's compute_effective_datadir
// → datadir_suffix). The writer creates the store here so a boot at
// --datadir=<datadir> finds it directly, without triggering ethrex's datadir
// auto-migration (which fs::renames every datadir entry — including the genesis
// sidecar and JWT — into the subdir). The chain-id default mirrors buildGenesisJSON.
func StoreDir(datadir string, g *genesis.Genesis) string {
	chainID := int64(1337)
	if g != nil && g.Config != nil && g.Config.ChainID != nil {
		chainID = g.Config.ChainID.Int64()
	}
	return filepath.Join(datadir, fmt.Sprintf("chain-%d", chainID))
}

// GenesisFileName is the filename for the ethrex-bootable genesis JSON
// written next to the DB so boot scripts can pass --network=<path>.
const GenesisFileName = "ethrex-genesis.json"

// MetadataFileName is the on-disk filename of the schema metadata file.
const MetadataFileName = "metadata.json"

// WriteStoreMetadata writes {"schema_version": 2} to <dbPath>/metadata.json.
// This file is REQUIRED by ethrex's Store::new: when the datadir is non-empty
// but has no metadata.json, ethrex returns NotFoundDBVersion and refuses to
// boot. state-actor must write it because it pre-fills the dir with SST files.
func WriteStoreMetadata(dbPath string) error {
	type meta struct {
		SchemaVersion int `json:"schema_version"`
	}
	payload, err := json.Marshal(meta{SchemaVersion: ethrexinternal.StoreSchemaVersion})
	if err != nil {
		return fmt.Errorf("ethrex: marshal metadata.json: %w", err)
	}
	outPath := filepath.Join(dbPath, MetadataFileName)
	if err := os.WriteFile(outPath, payload, 0o644); err != nil {
		return fmt.Errorf("ethrex: write metadata.json: %w", err)
	}
	return nil
}

// WriteGenesisSidecar writes an ethrex-bootable genesis JSON to
// <dbPath>/ethrex-genesis.json. The file is used with `ethrex --network
// <path>` at boot. ethrex rewrites chain_data[ChainConfig] from this file
// on every start (set_chain_config runs before the boot-gate check), so the
// DB's chain_data entry and this sidecar must be consistent.
func WriteGenesisSidecar(dbPath string, g *genesis.Genesis) error {
	payload, err := buildGenesisJSON(g)
	if err != nil {
		return fmt.Errorf("ethrex: build genesis sidecar: %w", err)
	}
	outPath := filepath.Join(dbPath, GenesisFileName)
	if err := os.WriteFile(outPath, payload, 0o644); err != nil {
		return fmt.Errorf("ethrex: write genesis sidecar: %w", err)
	}
	return nil
}

// ChainConfigJSON returns the chain_data[0x80] value bytes: ethrex's
// serde_json serialisation of the chain config. ethrex always overwrites
// this field from --network on boot (set_chain_config runs before the boot-gate
// check), so byte-exactness here is non-critical; valid camelCase JSON suffices.
func ChainConfigJSON(g *genesis.Genesis) ([]byte, error) {
	if g == nil || g.Config == nil {
		return nil, fmt.Errorf("ethrex: ChainConfigJSON: nil genesis or genesis.Config")
	}
	cfg := buildChainConfigMap(g)
	return json.Marshal(cfg)
}

// buildGenesisJSON builds the full ethrex genesis JSON from cfg.Genesis.
// The shape matches what ethrex deserializes for --network: config(camelCase),
// alloc, coinbase, difficulty, gasLimit, timestamp, extraData, nonce, mixHash,
// plus fork-time fields that are present.
func buildGenesisJSON(g *genesis.Genesis) ([]byte, error) {
	if g == nil || g.Config == nil {
		return nil, fmt.Errorf("buildGenesisJSON: nil genesis or genesis.Config")
	}

	chainID := int64(1337)
	if g.Config.ChainID != nil {
		chainID = g.Config.ChainID.Int64()
	}
	gasLimit := uint64(g.GasLimit)
	if gasLimit == 0 {
		gasLimit = 30_000_000
	}

	cfg := buildChainConfigMap(g)
	cfg["chainId"] = chainID

	genesisDoc := map[string]any{
		"config":     cfg,
		"nonce":      fmt.Sprintf("0x%x", uint64(g.Nonce)),
		"timestamp":  fmt.Sprintf("0x%x", uint64(g.Timestamp)),
		"gasLimit":   fmt.Sprintf("0x%x", gasLimit),
		"difficulty": "0x0",
		"mixHash":    g.Mixhash.Hex(),
		"coinbase":   g.Coinbase.Hex(),
		// alloc is intentionally empty: state-actor writes the account / storage
		// trie CFs directly and stamps the real state root into the genesis
		// header itself, so the sidecar must not carry accounts (ethrex would
		// otherwise try to re-initialize them).
		//
		// IMPORTANT: on boot ethrex recomputes the genesis state root from this
		// alloc and compares it to the stored header. With an empty alloc that
		// yields EMPTY_TRIE_HASH, which never matches the real stored root, so
		// `add_initial_state` returns IncompatibleChainConfig and refuses to
		// boot. ethrex must therefore be launched with --skip-genesis-validation
		// (lambdaclass/ethrex#6783) to trust the stored root; TestE2ESuite wires
		// that flag into the boot args.
		"alloc": map[string]any{},
	}
	if len(g.ExtraData) > 0 {
		genesisDoc["extraData"] = "0x" + bytesToHex(g.ExtraData)
	} else {
		genesisDoc["extraData"] = "0x"
	}
	if g.Difficulty != nil {
		genesisDoc["difficulty"] = fmt.Sprintf("0x%x", g.Difficulty.ToInt())
	}

	out, err := json.MarshalIndent(genesisDoc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("buildGenesisJSON marshal: %w", err)
	}
	return append(out, '\n'), nil
}

// buildChainConfigMap constructs the ethrex chain config map in camelCase.
// ethrex fills in additional fields (blobSchedule, bpoXTime defaults, etc.)
// from its own defaults on boot; we emit only what we know from the genesis.
func buildChainConfigMap(g *genesis.Genesis) map[string]any {
	cfg := map[string]any{
		"homesteadBlock":                0,
		"daoForkSupport":                false,
		"eip150Block":                   0,
		"eip155Block":                   0,
		"eip158Block":                   0,
		"byzantiumBlock":                0,
		"constantinopleBlock":           0,
		"petersburgBlock":               0,
		"istanbulBlock":                 0,
		"berlinBlock":                   0,
		"londonBlock":                   0,
		"mergeNetsplitBlock":            0,
		"terminalTotalDifficulty":       "0x0",
		"terminalTotalDifficultyPassed": true,
	}

	// Blob schedule from spec parameters.
	blobSchedule := map[string]any{
		"cancun": map[string]any{
			"baseFeeUpdateFraction": 3338477,
			"max":                   6,
			"target":                3,
		},
		"prague": map[string]any{
			"baseFeeUpdateFraction": 5007716,
			"max":                   9,
			"target":                6,
		},
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
		blobSchedule["osaka"] = map[string]any{
			"baseFeeUpdateFraction": 5007716,
			"max":                   9,
			"target":                6,
		}
	}

	cfg["blobSchedule"] = blobSchedule

	// depositContractAddress is a MANDATORY field in ethrex's ChainConfig
	// deserializer (no serde default), so it must always be present in the
	// sidecar or `ethrex --network <file>` fails to decode the genesis. Use the
	// genesis value when set; otherwise fall back to the mainnet deposit contract
	// (which is what ethrex serializes into chain_data by default — see the
	// golden genesis_dump.json chain config).
	depositAddr := g.Config.DepositContractAddress
	if depositAddr == (common.Address{}) {
		depositAddr = common.HexToAddress(defaultDepositContractAddress)
	}
	// .Hex() emits the EIP-55 checksummed (mixed-case) form; ethrex's address
	// deserializer accepts any case, so this round-trips fine.
	cfg["depositContractAddress"] = depositAddr.Hex()

	return cfg
}

// defaultDepositContractAddress is the mainnet beacon deposit contract, used as
// the sidecar's depositContractAddress when the genesis config leaves it unset.
const defaultDepositContractAddress = "0x00000000219ab540356cbb839cbe05303d7705fa"

func bytesToHex(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexChars[c>>4]
		out[i*2+1] = hexChars[c&0x0F]
	}
	return string(out)
}
