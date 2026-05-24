//go:build cgo_reth

package reth

import (
	"bytes"
	"context"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/entitygen"
	iReth "github.com/nerolation/state-actor/internal/reth"
	"github.com/nerolation/state-actor/internal/streamsort"
)

const defaultStreamBatchSize = 100_000

// contractStreamBatchSize sets the MDBX commit cadence for the contract
// loop. Smaller than defaultStreamBatchSize because per-contract writes
// (≈35 KiB mean storage under the 20/10/70 auto-fill split) are much
// larger than per-EOA writes, so a 1K batch keeps txn working-set RAM
// bounded.
const contractStreamBatchSize = 1_000

// runCgoNotAvailableError is nil under -tags cgo_reth (kept as a symbol
// so TestRunCgoStubBuildPath compiles in both build modes).
var runCgoNotAvailableError error = nil

// emptyMPTRoot is keccak256(rlp([])) — the canonical empty MPT root.
var emptyMPTRoot = common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

// RunCgo is the cgo direct-write entry point for --client=reth. Builds
// a reth-compatible datadir end-to-end without spawning the reth binary.
//
// On error, partially written files in cfg.DBPath are NOT cleaned up;
// the freshDir precondition rejects the next invocation until the
// caller manually removes the directory.
func RunCgo(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("RunCgo: cfg.DBPath required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DBPath, 0o755); err != nil {
		return nil, fmt.Errorf("RunCgo: mkdir datadir: %w", err)
	}

	envs, err := OpenEnvs(cfg.DBPath, true)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: OpenEnvs: %w", err)
	}
	defer envs.Close()

	if err := WriteDatabaseVersion(filepath.Join(cfg.DBPath, "db")); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteDatabaseVersion: %w", err)
	}

	stateRoot := emptyMPTRoot
	accountsCreated := 0
	contractsCreated := 0
	stats := &generator.Stats{}

	// Sorter is colocated with the datadir so its disk budget is shared.
	sorter, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: streamsort.New: %w", err)
	}
	defer sorter.Close()

	putAccountRLP := func(acc *entitygen.Account) error {
		rlpBytes, err := rlp.EncodeToBytes(acc.StateAccount)
		if err != nil {
			return fmt.Errorf("RLP encode %s: %w", acc.Address.Hex(), err)
		}
		return sorter.Put(acc.AddrHash[:], rlpBytes)
	}

	const batchSize = defaultStreamBatchSize

	if err := streamSpecStorage(ctx, envs, &cfg, stats); err != nil {
		return nil, fmt.Errorf("RunCgo: streamSpecStorage: %w", err)
	}

	// Alloc dispatch by shape: plain EOAs (empty Code AND Storage) go to
	// WriteEOAs (BytecodeHash=nil); contracts and 7702-delegating EOAs
	// (with 23-byte 0xef0100<addr> code) go to WriteContracts.
	if len(cfg.GenesisAccounts) > 0 {
		allocAccounts := buildAllocAccounts(cfg)
		var allocEOAs, allocContracts []*entitygen.Account
		for _, acc := range allocAccounts {
			if len(acc.Code) == 0 && len(acc.Storage) == 0 {
				allocEOAs = append(allocEOAs, acc)
			} else {
				allocContracts = append(allocContracts, acc)
			}
		}
		if len(allocEOAs) > 0 {
			if err := WriteEOAs(envs, allocEOAs, 0, cfg.Archive, stats); err != nil {
				return nil, fmt.Errorf("RunCgo: WriteEOAs(alloc): %w", err)
			}
		}
		if len(allocContracts) > 0 {
			if err := WriteContracts(envs, allocContracts, 0, cfg.Archive, stats); err != nil {
				return nil, fmt.Errorf("RunCgo: WriteContracts(alloc): %w", err)
			}
		}
		for _, acc := range allocAccounts {
			if err := putAccountRLP(acc); err != nil {
				return nil, fmt.Errorf("RunCgo: putAccountRLP(alloc): %w", err)
			}
		}
		accountsCreated += len(allocAccounts)
	}

	plan := cfg.AutoFill
	if plan != nil && (plan.NumEOAs > 0 || plan.NumContracts > 0) {
		rng := mrand.New(mrand.NewSource(cfg.Seed))

		remaining := plan.NumEOAs
		for remaining > 0 {
			b := batchSize
			if remaining < b {
				b = remaining
			}
			batch := make([]*entitygen.Account, b)
			for i := 0; i < b; i++ {
				batch[i] = plan.DrawEOA(rng)
			}
			if err := WriteEOAs(envs, batch, 0, cfg.Archive, stats); err != nil {
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

		if plan.NumContracts > 0 {
			remaining := plan.NumContracts
			for remaining > 0 {
				b := contractStreamBatchSize
				if remaining < b {
					b = remaining
				}
				batch := make([]*entitygen.Account, b)
				for i := 0; i < b; i++ {
					batch[i] = plan.DrawContract(rng)
				}
				// WriteContracts mutates each contract's StateAccount.Root
				// + .CodeHash in place; putAccountRLP below captures the
				// updated values.
				if err := WriteContracts(envs, batch, 0, cfg.Archive, stats); err != nil {
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

	if accountsCreated+contractsCreated > 0 {
		// Wrap state-root computation in an MDBX write txn so per-branch
		// emissions populate AccountsTrie; without it, reth's payload builder
		// falls back to a linear HashedAccounts walk per block. cursor.Put
		// (flag=0) accepts any key order: HashBuilder emissions arrive in
		// descending-depth order during unwinds, and the variable-length
		// AccountsTrie key layout means shallow paths sort smaller than
		// deeper ones — cursor.Append would silently drop shallow rows.
		var root common.Hash
		err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
			cur, cerr := txn.OpenCursor(envs.MdbxDBIs["AccountsTrie"])
			if cerr != nil {
				return fmt.Errorf("open AccountsTrie cursor: %w", cerr)
			}
			defer cur.Close()
			// AccountsTrie key uses the VARIABLE-length StoredNibbles
			// encoding (path.Nibbles[:path.Length], 0..=64 bytes, no
			// padding, no length suffix). This matches reth's
			// tables::AccountsTrie Key = StoredNibbles where
			// Encode::Encoded = ArrayVec<u8, 64> (see
			// reth/crates/storage/db-api/src/models/mod.rs:121-127).
			// Using the fixed 65-byte form (path.EncodeKey) would
			// produce keys reth misreads as 65-NIBBLE paths and the
			// walker eventually SIGSEGVs at block-time.
			emit := func(path iReth.StoredNibbles, node iReth.BranchNodeCompact) error {
				var keyBuf, valBuf bytes.Buffer
				path.EncodeAccountKey(&keyBuf)
				node.EncodeCompact(&valBuf)
				return cur.Put(keyBuf.Bytes(), valBuf.Bytes(), 0)
			}
			r, rerr := ComputeStateRootStreaming(sorter.Iterate, emit)
			if rerr != nil {
				return rerr
			}
			root = r
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("RunCgo: ComputeStateRootStreaming + AccountsTrie emit: %w", err)
		}
		stateRoot = root
	}

	// Free the sorter's temp files before the chainspec/static-files writes.
	if err := sorter.Close(); err != nil {
		return nil, fmt.Errorf("RunCgo: sorter.Close: %w", err)
	}

	gen := genesis.OrDefault(cfg.Genesis)
	chainID := gen.Config.ChainID.Int64()

	chainspecPath := filepath.Join(cfg.DBPath, "chainspec.json")
	if err := writeChainSpec(gen, chainspecPath); err != nil {
		return nil, fmt.Errorf("RunCgo: writeChainSpec: %w", err)
	}

	header, err := buildBlock0Header(gen)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: buildBlock0Header: %w", err)
	}
	header.Root = stateRoot

	if err := WriteMetadata(envs, header, uint64(chainID), cfg.Archive); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteMetadata: %w", err)
	}

	if err := WriteStaticFiles(cfg.DBPath, header); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteStaticFiles: %w", err)
	}

	stats.StateRoot = stateRoot
	stats.AccountsCreated = accountsCreated
	stats.ContractsCreated = contractsCreated
	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return stats, nil
}

