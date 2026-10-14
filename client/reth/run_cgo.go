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
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
	"github.com/ethereum/state-actor/internal/entitygen"
	"github.com/ethereum/state-actor/internal/manifest"
	iReth "github.com/ethereum/state-actor/internal/reth"
	"github.com/ethereum/state-actor/internal/streamsort"
)

const defaultStreamBatchSize = 100_000

// allocContractChunk sets how many alloc contracts have their code in
// memory at a time: 10 000 × 64 KiB ≈ 640 MiB at the largest pattern.
const allocContractChunk = 10_000

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

	fc, err := newFillController(envs.Mdbx, opts.FillTargets)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: newFillController: %w", err)
	}
	envs.Fill = fc

	if err := WriteDatabaseVersion(filepath.Join(cfg.DBPath, "db")); err != nil {
		return nil, fmt.Errorf("RunCgo: WriteDatabaseVersion: %w", err)
	}

	stateRoot := emptyMPTRoot
	accountsCreated := 0
	contractsCreated := 0
	stats := &generator.Stats{
		MdbxFill: &manifest.MdbxFillReport{
			Profile:        opts.FillProfileName,
			TableFill:      opts.FillTargets,
			TableReserve:   fc.reserves,
			LibmdbxVersion: mdbx.Version(),
		},
	}

	// Sorter is colocated with the datadir so its disk budget is shared.
	sorter, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: streamsort.New: %w", err)
	}
	defer sorter.Close()

	// trieSorter spills StoragesTrie rows (addrHash || packed SubKey || BNC,
	// see storagesTrieEmitter) so contracts and PreAlloc storage entities can
	// write in generation order while drainStoragesTrie appends them to the
	// table addrHash-sorted. Shared by WriteContracts and streamSpecStorage;
	// the latter's workers Put concurrently, serialized under the store's
	// putMu (see streamsort package doc).
	trieSorter, err := streamsort.New(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: streamsort.New(trie): %w", err)
	}
	defer trieSorter.Close()

	// putAccountRLP sorter-keys each account by AddrHash with a framed value:
	// [1-byte len(compact)] [compact HashedAccounts row] [account RLP], split
	// again in phase 2. isContract must match the WriteEOAs/WriteContracts
	// dispatch (WriteContracts must have run so StateAccount.CodeHash is set).
	putAccountRLP := func(acc *entitygen.Account, isContract bool) error {
		rlpBytes, err := rlp.EncodeToBytes(acc.StateAccount)
		if err != nil {
			return fmt.Errorf("RLP encode %s: %w", acc.Address.Hex(), err)
		}
		compact := hashedAccountValue(acc, isContract)
		frame := make([]byte, 0, 1+len(compact)+len(rlpBytes))
		frame = append(frame, byte(len(compact)))
		frame = append(frame, compact...)
		frame = append(frame, rlpBytes...)
		if err := sorter.Put(acc.AddrHash[:], frame); err != nil {
			return err
		}
		stats.AccountBytes += uint64(len(compact))
		return nil
	}

	const batchSize = defaultStreamBatchSize

	// Giants, largest first (Giant(seed,k) is pure in k; see internal/autofill/giants.go).
	var giants []*autofill.Giant
	if cfg.AutoFill != nil {
		for k := range cfg.AutoFill.Giants {
			giants = append(giants, cfg.AutoFill.Giant(cfg.Seed, k))
		}
	}
	giantRoots, err := streamSpecStorage(ctx, envs, trieSorter, &cfg, stats, giants)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: streamSpecStorage: %w", err)
	}

	// Alloc dispatch by shape: plain EOAs (empty Code AND Storage) are
	// sorter-keyed with isContract=false; contracts and code-bearing EOAs
	// go to WriteContracts.
	if len(cfg.GenesisAccounts) > 0 {
		allocAccounts := buildAllocAccounts(cfg)
		var allocEOAs, allocContracts []*entitygen.Account
		for _, acc := range allocAccounts {
			if cfg.CodeSizeAt(acc.Address) == 0 && len(acc.Storage) == 0 {
				allocEOAs = append(allocEOAs, acc)
			} else {
				allocContracts = append(allocContracts, acc)
			}
		}
		if cfg.Archive && len(allocEOAs) > 0 {
			if err := WriteEOAs(envs, allocEOAs, 0); err != nil {
				return nil, fmt.Errorf("RunCgo: WriteEOAs(alloc): %w", err)
			}
		}
		// buildAllocAccounts leaves lazy code out. Make it one chunk at a
		// time and drop it after the chunk is written, so the runtimes of
		// all alloc contracts are never in memory together.
		for start := 0; start < len(allocContracts); start += allocContractChunk {
			chunk := allocContracts[start:min(start+allocContractChunk, len(allocContracts))]
			for _, acc := range chunk {
				if acc.Code == nil {
					acc.Code = cfg.CodeAt(acc.Address)
				}
			}
			if err := WriteContracts(envs, trieSorter, chunk, 0, cfg.Archive, stats); err != nil {
				return nil, fmt.Errorf("RunCgo: WriteContracts(alloc): %w", err)
			}
			for _, acc := range chunk {
				acc.Code = nil
			}
		}
		for _, acc := range allocEOAs {
			if err := putAccountRLP(acc, false); err != nil {
				return nil, fmt.Errorf("RunCgo: putAccountRLP(alloc EOA): %w", err)
			}
		}
		for _, acc := range allocContracts {
			if err := putAccountRLP(acc, true); err != nil {
				return nil, fmt.Errorf("RunCgo: putAccountRLP(alloc contract): %w", err)
			}
		}
		accountsCreated += len(allocAccounts)
	}

	plan := cfg.AutoFill
	if plan != nil && (plan.NumEOAs > 0 || plan.NumContracts > 0) {
		rng := mrand.New(mrand.NewSource(cfg.Seed))

		cfg.Progress.Stage("reth: generating accounts")

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
			// plan.DrawEOA can return storage-less EOAs carrying code (7702
			// designators or, under --account-model=mainnet, arbitrary code).
			// They go through WriteContracts (Bytecodes row, CodeHash set in
			// place) and are sorter-keyed with isContract=true so their
			// HashedAccounts row carries the code hash the state root commits to.
			var plainEOAs, codeEOAs []*entitygen.Account
			for _, acc := range batch {
				if len(acc.Code) == 0 {
					plainEOAs = append(plainEOAs, acc)
				} else {
					codeEOAs = append(codeEOAs, acc)
				}
			}
			if cfg.Archive && len(plainEOAs) > 0 {
				if err := WriteEOAs(envs, plainEOAs, 0); err != nil {
					return nil, fmt.Errorf("RunCgo: WriteEOAs: %w", err)
				}
			}
			if len(codeEOAs) > 0 {
				if err := WriteContracts(envs, trieSorter, codeEOAs, 0, cfg.Archive, stats); err != nil {
					return nil, fmt.Errorf("RunCgo: WriteContracts(code EOA): %w", err)
				}
			}
			for _, acc := range plainEOAs {
				if err := putAccountRLP(acc, false); err != nil {
					return nil, fmt.Errorf("RunCgo: putAccountRLP(EOA): %w", err)
				}
			}
			for _, acc := range codeEOAs {
				if err := putAccountRLP(acc, true); err != nil {
					return nil, fmt.Errorf("RunCgo: putAccountRLP(code EOA): %w", err)
				}
			}
			accountsCreated += b
			remaining -= b
			cfg.Progress.Tick(int64(plan.NumEOAs-remaining), int64(plan.NumEOAs), "EOAs")
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
				if err := WriteContracts(envs, trieSorter, batch, 0, cfg.Archive, stats); err != nil {
					return nil, fmt.Errorf("RunCgo: WriteContracts: %w", err)
				}
				for _, c := range batch {
					if err := putAccountRLP(c, true); err != nil {
						return nil, fmt.Errorf("RunCgo: putAccountRLP(contract): %w", err)
					}
				}
				contractsCreated += b
				remaining -= b
				cfg.Progress.Tick(int64(plan.NumContracts-remaining), int64(plan.NumContracts), "contracts")
			}
		}
	}

	// Giants' storage was streamed in Phase 0; only the account (nonce,
	// balance, code, precomputed root) goes through the normal contract
	// path. Not added to cfg.GenesisAccounts — WriteContracts preserves
	// the preset Root since Storage is empty.
	for _, g := range giants {
		root, ok := giantRoots[g.AddrHash]
		if !ok {
			return nil, fmt.Errorf("RunCgo: giant %s missing phase0 root", g.Address.Hex())
		}
		acc := &entitygen.Account{
			Address:  g.Address,
			AddrHash: g.AddrHash,
			StateAccount: &types.StateAccount{
				Nonce:   g.Nonce,
				Balance: g.Balance,
				Root:    root,
			},
			Code: g.Code,
		}
		if err := WriteContracts(envs, trieSorter, []*entitygen.Account{acc}, 0, cfg.Archive, stats); err != nil {
			return nil, fmt.Errorf("RunCgo: WriteContracts(giant %s): %w", g.Address.Hex(), err)
		}
		if err := putAccountRLP(acc, true); err != nil {
			return nil, fmt.Errorf("RunCgo: putAccountRLP(giant): %w", err)
		}
		contractsCreated++
	}

	// Drain StoragesTrie now that every contract and PreAlloc entity has
	// written its rows (WriteContracts + streamSpecStorage, both above).
	// Explicit Close (not just the defer) frees its temp files before the
	// chainspec/static-files writes, mirroring the account sorter below.
	cfg.Progress.Stage("reth: draining StoragesTrie")
	if err := drainStoragesTrie(envs, trieSorter); err != nil {
		return nil, fmt.Errorf("RunCgo: drainStoragesTrie: %w", err)
	}
	if err := trieSorter.Close(); err != nil {
		return nil, fmt.Errorf("RunCgo: trieSorter.Close: %w", err)
	}

	if accountsCreated+contractsCreated > 0 {
		// Persist HashBuilder trie-node emissions into AccountsTrie alongside
		// state-root computation so reth's payload builder finds them on
		// boot. Keys use the 33-byte PackedStoredNibbles form (reth v2);
		// cursor.Put is correct regardless of emission order.
		//
		// HashedAccounts rows are also written here, via cursor.Append in
		// the addrHash-sorted order the sorter yields. Append requires
		// ascending keys, so it packs MDBX's leaf pages densely (libmdbx
		// >= v0.10.0) and fails on out-of-order input.
		cfg.Progress.Stage("reth: phase 2/2 — building account trie")
		phase2Total := int64(accountsCreated + contractsCreated)
		var phase2Count int64
		var root common.Hash
		err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
			cur, cerr := txn.OpenCursor(envs.MdbxDBIs["AccountsTrie"])
			if cerr != nil {
				return fmt.Errorf("open AccountsTrie cursor: %w", cerr)
			}
			defer cur.Close()
			if ferr := envs.useFill("HashedAccounts"); ferr != nil {
				return fmt.Errorf("RunCgo: useFill(HashedAccounts): %w", ferr)
			}
			gw := &trieGroupWriter{put: func(rows []trieRow) error {
				if ferr := envs.useFill("AccountsTrie"); ferr != nil {
					return fmt.Errorf("useFill(AccountsTrie): %w", ferr)
				}
				for _, r := range rows {
					if err := cur.Put(r.key, r.val, 0); err != nil {
						return err
					}
				}
				if ferr := envs.useFill("HashedAccounts"); ferr != nil {
					return fmt.Errorf("useFill(HashedAccounts): %w", ferr)
				}
				return nil
			}}
			emit := func(path iReth.StoredNibbles, node iReth.BranchNodeCompact) error {
				if path.Length == 0 {
					// Skip root branch — reth's writer never caches it
					// (provider.rs `if !key.is_empty()`). See
					// TestNoRootCacheRows for the proof_v2 panic rationale.
					return nil
				}
				var keyBuf, valBuf bytes.Buffer
				path.EncodePackedAccountKey(&keyBuf)
				node.EncodeCompact(&valBuf)
				return gw.add(path, keyBuf.Bytes(), valBuf.Bytes())
			}

			hashedCur, herr := txn.OpenCursor(envs.MdbxDBIs["HashedAccounts"])
			if herr != nil {
				return fmt.Errorf("open HashedAccounts cursor: %w", herr)
			}
			defer hashedCur.Close()
			if _, _, err := hashedCur.Get(nil, nil, mdbx.First); err == nil {
				return fmt.Errorf("HashedAccounts is not empty before phase 2 — every row must come from the sorted Append path")
			} else if !mdbx.IsNotFound(err) {
				return fmt.Errorf("HashedAccounts empty-check: %w", err)
			}

			// Count-and-tick wrapper around the sorted account stream so the
			// pure-Go account-trie build shows live progress. Single-goroutine
			// (HashBuilder consumes the yield sequentially), so phase2Count is
			// race-free. Splits each putAccountRLP frame: compact → Append,
			// RLP → yield unchanged.
			countingIter := func(yield func(addrHash, accountRLP []byte) error) error {
				return sorter.Iterate(func(addrHash, frame []byte) error {
					if len(frame) < 1 {
						return fmt.Errorf("sorter frame for %x has no length prefix", addrHash)
					}
					n := int(frame[0])
					if len(frame) < 1+n {
						return fmt.Errorf("sorter frame for %x truncated compact value (want %d bytes)", addrHash, n)
					}
					compact, accountRLP := frame[1:1+n], frame[1+n:]
					if err := hashedCur.Put(addrHash, compact, mdbx.Append); err != nil {
						return fmt.Errorf("HashedAccounts append %x: %w", addrHash, err)
					}
					phase2Count++
					cfg.Progress.Tick(phase2Count, phase2Total, "accounts")
					return yield(addrHash, accountRLP)
				})
			}
			r, rerr := ComputeStateRootStreaming(countingIter, emit)
			if rerr != nil {
				return rerr
			}
			if ferr := gw.flush(); ferr != nil {
				return fmt.Errorf("RunCgo: flush AccountsTrie group: %w", ferr)
			}
			root = r
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("RunCgo: ComputeStateRootStreaming + AccountsTrie/HashedAccounts emit: %w", err)
		}
		stateRoot = root
	}

	// Free the sorter's temp files before the chainspec/static-files writes.
	if err := sorter.Close(); err != nil {
		return nil, fmt.Errorf("RunCgo: sorter.Close: %w", err)
	}

	gen := genesis.OrDefault(cfg.Genesis)

	chainspecPath := filepath.Join(cfg.DBPath, "chainspec.json")
	if err := writeChainSpec(gen, chainspecPath); err != nil {
		return nil, fmt.Errorf("RunCgo: writeChainSpec: %w", err)
	}

	header, err := buildBlock0Header(gen)
	if err != nil {
		return nil, fmt.Errorf("RunCgo: buildBlock0Header: %w", err)
	}
	header.Root = stateRoot

	if err := WriteMetadata(envs, header, cfg.Archive); err != nil {
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

// hashedAccountValue returns the Compact-encoded HashedAccounts row for acc:
// BytecodeHash=nil for isContract=false, else &acc.StateAccount.CodeHash.
func hashedAccountValue(acc *entitygen.Account, isContract bool) []byte {
	ethAccount := iReth.Account{
		Nonce:   acc.StateAccount.Nonce,
		Balance: acc.StateAccount.Balance,
	}
	if isContract {
		var h common.Hash
		copy(h[:], acc.StateAccount.CodeHash)
		ethAccount.BytecodeHash = &h
	}
	var buf bytes.Buffer
	ethAccount.EncodeCompact(&buf)
	return buf.Bytes()
}
