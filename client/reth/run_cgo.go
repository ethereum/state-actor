//go:build cgo_reth

package reth

import (
	"context"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/entitygen"
)

// defaultStreamBatchSize is the per-iteration generation batch when
// cfg.BatchSize is zero. Sized so one batch of pointers ≈ 20 MiB
// (100_000 × ~200 B) — comfortably below the 64 MiB Pebble flush
// threshold so even worst-case allocator slack stays in budget.
const defaultStreamBatchSize = 100_000

// runCgoNotAvailableError is nil under -tags cgo_reth. Kept as a symbol so
// TestRunCgoStubBuildPath compiles in both build modes.
var runCgoNotAvailableError error = nil

// emptyMPTRoot is the canonical Merkle-Patricia trie root of the empty trie:
// keccak256(rlp([])) = 0x56e81f17...
var emptyMPTRoot = common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

// RunCgo is the cgo direct-write entry point for --client=reth. Builds a
// reth-compatible datadir end-to-end without spawning the reth binary.
//
// Phases:
//  1. Pre-flight: DBPath required, mkdir, freshDir precondition (via OpenEnvs)
//  2. OpenEnvs (MDBX env + RocksDB CFs)
//  3. WriteDatabaseVersion sidecar
//  4. Streaming synthetic-account generation. Memory bounded by one batch
//     (~cfg.BatchSize accounts) plus Pebble's 64 MiB write buffer,
//     regardless of total N. Mirrors client/nethermind/entitygen_cgo.go.
//     a. Inject pre-funded accounts (cfg.InjectAddresses).
//     b. Synthetic EOAs in batches of cfg.BatchSize (default 100K).
//     c. Synthetic contracts in batches of cfg.BatchSize. WriteContracts
//        mutates each contract's StateAccount.Root + .CodeHash IN-PLACE
//        before the per-account RLP is written into the sorter, so the
//        global state root sees the correct trie/code linkage.
//     d. Drain the Pebble sorter (ascending addrHash order) into the
//        streaming HashBuilder for the global state root.
//     Empty alloc (no inject + NumAccounts=0 + NumContracts=0) yields the
//     canonical empty-MPT hash 0x56e81f17...
//  5. Persist chainspec.json (still O(N) RAM — separate follow-up plan
//     covers the chainspec workaround).
//  6. Build genesis header with computed state root + WriteMetadata (5 tables)
//  7. WriteStaticFiles (block-0 segment files)
//  8. Return Stats (Close deferred)
//
// On error, partially written files in cfg.DBPath are NOT cleaned up; the
// freshDir precondition will reject the next invocation until the caller
// manually removes the directory.
func RunCgo(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("RunCgo: cfg.DBPath required")
	}
	if err := os.MkdirAll(cfg.DBPath, 0o755); err != nil {
		return nil, fmt.Errorf("RunCgo: mkdir datadir: %w", err)
	}

	// Phase 2: open MDBX + RocksDB.
	envs, err := OpenEnvs(cfg.DBPath, true)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: OpenEnvs: %w", err)
	}
	defer envs.Close()

	// Phase 3: write <dbDir>/database.version sidecar.
	if err := WriteDatabaseVersion(filepath.Join(cfg.DBPath, "db")); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteDatabaseVersion: %w", err)
	}

	// Phase 4: streaming synthetic-account generation.
	stateRoot := emptyMPTRoot
	accountsCreated := 0
	contractsCreated := 0

	// Pebble-backed sorter colocated with the datadir. The temp dir lives
	// under cfg.DBPath/reth-sort-* so it shares disk budget with the (often
	// large) datadir rather than competing with /tmp. Defer-Close runs on
	// every return path; the explicit Close before Phase 5 frees the disk
	// before chainspec/static-file writes (and is a no-op on the deferred
	// call due to idempotency).
	sorter, err := NewSorter(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: NewSorter: %w", err)
	}
	defer sorter.Close()

	// putAccountRLP encodes an account's StateAccount and writes it to the
	// sorter keyed by AddrHash. Used by all three sub-phases below. The
	// HashBuilder requires sorted-by-key input; Pebble's LSM auto-sorts on
	// iterate, so we can Put in any order here.
	putAccountRLP := func(acc *entitygen.Account) error {
		rlpBytes, err := rlp.EncodeToBytes(acc.StateAccount)
		if err != nil {
			return fmt.Errorf("RLP encode %s: %w", acc.Address.Hex(), err)
		}
		return sorter.Put(acc.AddrHash[:], rlpBytes)
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultStreamBatchSize
	}

	// Phase 4a: inject pre-funded accounts (e.g. Anvil dev account
	// 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266) so spamoor and other test
	// harnesses have a known-funded sender. Each gets 999_999_999 ETH, nonce 0,
	// no code, no storage. Address is taken verbatim from cfg.InjectAddresses.
	if len(cfg.InjectAddresses) > 0 {
		injected := make([]*entitygen.Account, len(cfg.InjectAddresses))
		for i, addr := range cfg.InjectAddresses {
			injected[i] = buildInjectedAccount(addr)
		}
		if err := WriteEOAs(envs, injected, 0); err != nil {
			return nil, fmt.Errorf("RunCgo: WriteEOAs(injected): %w", err)
		}
		for _, acc := range injected {
			if err := putAccountRLP(acc); err != nil {
				return nil, fmt.Errorf("RunCgo: putAccountRLP(injected): %w", err)
			}
		}
		accountsCreated += len(cfg.InjectAddresses)
	}

	if cfg.NumAccounts > 0 || cfg.NumContracts > 0 {
		rng := mrand.New(mrand.NewSource(cfg.Seed))

		// Phase 4b: synthetic EOAs in batches of batchSize. The RNG is
		// drawn from in the same order as the legacy single-shot loop, so
		// state-root determinism is preserved (locked in by
		// TestStreaming_GoldenEqualsLegacy).
		remaining := cfg.NumAccounts
		for remaining > 0 {
			b := batchSize
			if remaining < b {
				b = remaining
			}
			batch := make([]*entitygen.Account, b)
			for i := 0; i < b; i++ {
				batch[i] = entitygen.GenerateEOA(rng)
			}
			if err := WriteEOAs(envs, batch, 0); err != nil {
				return nil, fmt.Errorf("RunCgo: WriteEOAs: %w", err)
			}
			for _, acc := range batch {
				if err := putAccountRLP(acc); err != nil {
					return nil, fmt.Errorf("RunCgo: putAccountRLP(EOA): %w", err)
				}
			}
			accountsCreated += b
			remaining -= b
		}

		// Phase 4c: synthetic contracts in batches of batchSize. Slot count
		// is drawn per-contract via entitygen.GenerateSlotCount so the RNG
		// draw sequence matches besu/nethermind/geth — same --seed →
		// identical canonical entitygen MPT root across MPT clients
		// (locked in by client/reth/golden_test.go).
		if cfg.NumContracts > 0 {
			codeSize := cfg.CodeSize
			if codeSize <= 0 {
				codeSize = 256
			}
			remaining := cfg.NumContracts
			for remaining > 0 {
				b := batchSize
				if remaining < b {
					b = remaining
				}
				batch := make([]*entitygen.Account, b)
				for i := 0; i < b; i++ {
					slotCount := entitygen.GenerateSlotCount(rng, cfg.Distribution, cfg.MinSlots, cfg.MaxSlots)
					batch[i] = entitygen.GenerateContract(rng, codeSize, slotCount)
				}
				// WriteContracts mutates each contract's StateAccount.Root
				// (storage trie root) and .CodeHash in-place BEFORE
				// returning, so the per-account RLP-encode below captures
				// the correct values for the global state trie.
				if err := WriteContracts(envs, batch, 0); err != nil {
					return nil, fmt.Errorf("RunCgo: WriteContracts: %w", err)
				}
				for _, c := range batch {
					if err := putAccountRLP(c); err != nil {
						return nil, fmt.Errorf("RunCgo: putAccountRLP(contract): %w", err)
					}
				}
				contractsCreated += b
				remaining -= b
			}
		}
	}

	// Phase 4d: drain sorter (ascending addrHash) into the HashBuilder.
	if accountsCreated+contractsCreated > 0 {
		root, err := ComputeStateRootStreaming(sorter.Iterate)
		if err != nil {
			return nil, fmt.Errorf("RunCgo: ComputeStateRootStreaming: %w", err)
		}
		stateRoot = root
	}

	// Free the sorter (and its temp Pebble files) before Phase 5 so disk
	// budget is reclaimed for chainspec.json + static-files writes.
	if err := sorter.Close(); err != nil {
		return nil, fmt.Errorf("RunCgo: sorter.Close: %w", err)
	}

	// Phase 5a: resolve genesis + chainID, persist chainspec.json. The
	// chainspec is now alloc-free (just config + header bits) — reth boots
	// with --debug.skip-genesis-validation and trusts the DB-resident
	// genesis state. File size is constant in N, no longer the OOM ceiling.
	gen := genesis.OrDefault(cfg.Genesis)
	chainID := gen.Config.ChainID.Int64()

	chainspecPath := filepath.Join(cfg.DBPath, "chainspec.json")
	if err := writeChainSpec(gen, chainspecPath); err != nil {
		return nil, fmt.Errorf("RunCgo: writeChainSpec: %w", err)
	}

	// Phase 5b: build genesis header (block 0) + write MDBX metadata tables.
	header, err := buildBlock0Header(gen)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: buildBlock0Header: %w", err)
	}
	// Use the computed state root (empty-MPT for no alloc, or trie root for accounts).
	header.Root = stateRoot

	if err := WriteMetadata(envs, header, uint64(chainID)); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteMetadata: %w", err)
	}

	// Phase 6: write block-0 static-files segments.
	if err := WriteStaticFiles(cfg.DBPath, header); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteStaticFiles: %w", err)
	}

	// Phase 7: return stats (envs.Close is deferred above).
	return &generator.Stats{
		StateRoot:        stateRoot,
		AccountsCreated:  accountsCreated,
		ContractsCreated: contractsCreated,
	}, nil
}
