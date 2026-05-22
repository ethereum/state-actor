package generator

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/autofill"
	"github.com/nerolation/state-actor/internal/templates"
)

// TrieMode represents the trie algorithm used for state root computation.
type TrieMode string

const (
	// TrieModeMPT uses the Merkle Patricia Trie (hexary, keccak256-based).
	TrieModeMPT TrieMode = "mpt"

	// TrieModeBinary uses the EIP-7864 binary trie.
	// Compatible with geth's --override.verkle=0 flag (=0 is the activation block number).
	TrieModeBinary TrieMode = "binary"
)

// Config holds the configuration for state generation.
type Config struct {
	// DBPath is the path to the Pebble database directory.
	DBPath string

	// AutoFill is the resolved synthetic-fill plan. Built from --target-size
	// (and the spec headroom when --spec is set) by internal/autofill, or by
	// tests calling autofill.PlanForBudget directly. nil means no synthetic
	// top-up runs — writers emit only PreAlloc + GenesisAccounts.
	AutoFill *autofill.Plan

	// Seed is the random seed for reproducible generation.
	Seed int64

	// Verbose enables verbose logging.
	Verbose bool

	// TrieMode selects the trie algorithm for state root computation.
	// Defaults to TrieModeMPT if empty.
	TrieMode TrieMode

	// Genesis is the in-memory genesis configuration that drives chainID,
	// fork selection, and the genesis-block header fields. Always non-nil
	// after main.go's BuildSynthetic call. Each client's writer reads
	// Genesis.Config (for IsLondon / IsShanghai / etc.) and Genesis's
	// header fields (GasLimit, Timestamp, ExtraData, ...).
	//
	// Genesis.Alloc is intentionally always empty in the production path:
	// pre-funded accounts come from --inject-accounts, not the genesis JSON.
	Genesis *genesis.Genesis

	// GenesisAccounts / GenesisStorage / GenesisCode carry pre-allocated
	// state consumed by all 4 client writers (e.g. post-Merge EIP system
	// contracts deployed by syscontracts.AddCanonicalSystemContracts). Validate()
	// rejects orphan Storage/Code entries (no matching GenesisAccounts).
	// The --spec YAML path populates these via PreAlloc + Validate().
	GenesisAccounts map[common.Address]*types.StateAccount
	GenesisStorage  map[common.Address]map[common.Hash]common.Hash
	GenesisCode     map[common.Address][]byte

	// CommitInterval is the number of accounts between binary trie commits
	// to disk. Only used in TrieModeBinary. 0 means no intermediate commits
	// (entire trie stays in memory). When set, the trie is periodically
	// committed to a temporary Pebble database and reopened, bounding memory
	// to the working set between commits (~1-2 GB) instead of the full trie.
	CommitInterval int

	// WriteTrieNodes enables writing serialized binary trie nodes to the
	// database during Phase 2 hash computation, making the database usable
	// by geth's PathDB. Automatically set when --genesis is provided.
	WriteTrieNodes bool

	// TargetSize is the target total database size on disk in bytes.
	// When set (> 0), writers stop emitting once the projected on-disk
	// size reaches this target. AutoFill.NumEOAs / NumContracts are the
	// upper bound on emission; the target-size stop kicks in earlier when
	// the budget is exhausted before the counts are.
	TargetSize uint64

	// GroupDepth is the binary trie group depth (1-8, default 8).
	// Controls how many trie levels are serialized per DB entry.
	GroupDepth int

	// PreAlloc carries entities produced by the --spec YAML translator
	// (internal/specbuild/). Validate() materializes their Account +
	// Code into GenesisAccounts/Code (Storage stays on the iter for
	// streaming consumption by per-client writers).
	PreAlloc []templates.PreAllocEntity

	// Archive, when true, generates a DB sized for an archive-mode node:
	// reth writes the four archive-only tables (Account/StorageChangeSets +
	// Account/StoragesHistory); geth writes the PathDB archive-anchor
	// metadata for --gcmode=archive. besu and nethermind have no archive
	// code path; main.go rejects --archive for those clients at parse time.
	Archive bool
}

// Validate materializes PreAlloc into the GenesisAccounts/Code maps,
// rejects orphan Storage/Code entries, and ensures the writer has at
// least one source of entities to emit. Must be called before any
// writer consumes Config.
func (c *Config) Validate() error {
	if err := c.materializePreAlloc(); err != nil {
		return err
	}

	for a := range c.GenesisCode {
		if _, ok := c.GenesisAccounts[a]; !ok {
			return fmt.Errorf("Config: GenesisCode[%s] has no corresponding GenesisAccounts entry (orphan code)", a.Hex())
		}
	}
	for a := range c.GenesisStorage {
		if _, ok := c.GenesisAccounts[a]; !ok {
			return fmt.Errorf("Config: GenesisStorage[%s] has no corresponding GenesisAccounts entry (orphan storage)", a.Hex())
		}
	}

	if c.AutoFill == nil && len(c.PreAlloc) == 0 && len(c.GenesisAccounts) == 0 {
		return fmt.Errorf("Config: no entities to emit — set AutoFill (via autofill.PlanForBudget) or supply PreAlloc/GenesisAccounts")
	}
	return nil
}

// materializePreAlloc folds PreAlloc account headers + code into
// GenesisAccounts/Code so writers consume both YAML-spec and
// programmatic alloc entries uniformly.
//
// Storage is NOT drained — it stays as iter.Seq2 on c.PreAlloc for
// per-client streaming Phases to consume. Writers read the computed
// storage root from c.GenesisAccounts[addr].Root after the streaming
// Phase splices it in.
//
// Re-Validate is idempotent: same-pointer collisions (`existing == pe.Account`)
// are skipped rather than treated as errors.
func (c *Config) materializePreAlloc() error {
	if len(c.PreAlloc) == 0 {
		return nil
	}
	if c.GenesisAccounts == nil {
		c.GenesisAccounts = make(map[common.Address]*types.StateAccount, len(c.PreAlloc))
	}
	if c.GenesisCode == nil {
		c.GenesisCode = make(map[common.Address][]byte)
	}

	for i, pe := range c.PreAlloc {
		if existing, dup := c.GenesisAccounts[pe.Address]; dup {
			if existing == pe.Account {
				continue
			}
			return fmt.Errorf("Config.PreAlloc[%d]: address %s already present in GenesisAccounts with a different *types.StateAccount pointer — either pass the same pointer (idempotent re-Validate is OK) or move the pre-existing entry into PreAlloc", i, pe.Address.Hex())
		}
		c.GenesisAccounts[pe.Address] = pe.Account
		if len(pe.Code) > 0 {
			c.GenesisCode[pe.Address] = pe.Code
		}
	}
	return nil
}

// Stats holds statistics about the generation process.
type Stats struct {
	// AccountsCreated is the number of accounts created.
	AccountsCreated int

	// ContractsCreated is the number of contracts created.
	ContractsCreated int

	// StorageSlotsCreated is the total number of storage slots created.
	StorageSlotsCreated int

	// TotalBytes is the total number of bytes written.
	TotalBytes uint64

	// AccountBytes is the number of bytes for account data.
	AccountBytes uint64

	// StorageBytes is the number of bytes for storage data.
	StorageBytes uint64

	// CodeBytes is the number of bytes for contract code.
	CodeBytes uint64

	// TrieNodeBytes is the number of bytes written for trie nodes (Phase 2).
	// Only populated when WriteTrieNodes is true.
	TrieNodeBytes uint64

	// StemBlobBytes is the number of bytes written for bintrie flat-state
	// stem blobs (Phase 2). Only populated in binary trie mode.
	StemBlobBytes uint64

	// StateRoot is the computed state root hash.
	StateRoot common.Hash

	// SampleEOAs holds a few sample EOA addresses for post-generation verification.
	SampleEOAs []common.Address

	// SampleContracts holds a few sample contract addresses for post-generation verification.
	SampleContracts []common.Address

	// DBWriteTime is the time spent writing to the database.
	DBWriteTime time.Duration

	// GenerationTime is the time spent generating data.
	GenerationTime time.Duration
}
