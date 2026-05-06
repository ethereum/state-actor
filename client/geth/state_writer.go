package geth

import (
	"bytes"
	"context"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
)

// phase1FlushBytes mirrors client/besu/state_writer_cgo.go: 64 MiB temp
// Pebble batch flush threshold, large enough to amortise commit overhead
// without exploding RAM.
const phase1FlushBytes = 64 * 1024 * 1024

// parallelKeccakThreshold matches the legacy MPT path: ≥ 64 storage
// slots in one contract triggers parallel keccak hashing for keys.
const parallelKeccakThreshold = 64

// writeStateAndCollectRoot drives the two-phase MPT pipeline for the
// geth client.
//
// Phase 1 streams entitygen output (genesis-alloc accounts, inject
// addresses, synthetic EOAs, synthetic contracts) into a temp Pebble DB
// keyed by addrHash; Pebble's LSM auto-sorts on key, so Phase 2 reads in
// keccak order without an explicit sort step. WAL is disabled on the
// temp DB — Phase 2 starts from a deterministic seed if Phase 1 crashes.
//
// Phase 2 forward-iterates the compacted scratch and writes the
// production geth Pebble in keccak order. For each entity it builds the
// per-account storage trie via trie.StackTrie, splices the storage root
// into the account, writes the snapshot entries, writes contract code
// (deduped by codeHash), and feeds (addrHash, full StateAccount RLP)
// into the outer account trie. trie.StackTrie's OnTrieNode callback
// emits trie nodes which we route to PathScheme keys
// (TrieNodeAccountPrefix / TrieNodeStoragePrefix) on the production DB.
//
// Returns the final state root and a populated *generator.Stats.
//
// Memory is bounded by O(max storage slots in any single contract): no
// goroutine accumulates the full account set, and the temp DB streams
// off disk. Sequential writes to the production DB give Pebble the
// best-case LSM workload — append-mostly, no compaction storm.
func writeStateAndCollectRoot(
	ctx context.Context,
	cfg generator.Config,
	w *Writer,
) (common.Hash, *generator.Stats, error) {
	stats := &generator.Stats{}
	start := time.Now()

	// --- Phase 1: stream entitygen → temp Pebble. ---

	tmpDir, err := os.MkdirTemp("", "geth-mpt-*")
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("geth: mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pdb, err := pebble.Open(tmpDir, &pebble.Options{
		MemTableSize: 64 * 1024 * 1024,
		// DisableWAL is left at default-false because pebble's compactor
		// can stall waiting on WAL fsyncs even with Sync=false on
		// individual writes; the dominant cost we're avoiding is sync,
		// not the WAL itself.
	})
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("geth: open temp pebble: %w", err)
	}
	defer pdb.Close()

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))
	pendingBytes := 0
	batch := pdb.NewBatch()
	flush := func() error {
		if err := batch.Commit(noSync); err != nil {
			return err
		}
		batch = pdb.NewBatch()
		pendingBytes = 0
		return nil
	}

	// Phase 1 raw-byte safety cap for --target-size. Phase 1 doesn't
	// write to the production DB (all prod writes happen in Phase 2 in
	// keccak order), so dirSize sampling here would be misleading. As
	// a coarse upper bound we stop entity emission once raw entity
	// bytes reach 5× cfg.TargetSize — that's enough headroom to let
	// Phase 2's accurate dirSize stop trigger before we waste work on
	// entities that will never get written to the production DB. The
	// 5× factor is empirical: MPT trie node overhead + storage tries
	// can push final DB size to ~3-4× raw entity bytes at high storage
	// density, so 5× ensures Phase 2 has enough material to hit the
	// target dirSize.
	totalRawBytes := uint64(0)
	targetReached := false
	const phase1Phase1RawSafetyMultiplier = 5
	checkTarget := func(blobLen int) bool {
		totalRawBytes += uint64(32 + blobLen)
		if cfg.TargetSize > 0 && totalRawBytes >= cfg.TargetSize*phase1Phase1RawSafetyMultiplier {
			if cfg.Verbose {
				log.Printf("geth MPT Phase 1 safety cap: raw bytes %d MiB >= 5× target %d MiB — stopping entity emission",
					totalRawBytes>>20, (cfg.TargetSize*phase1Phase1RawSafetyMultiplier)>>20)
			}
			targetReached = true
			return true
		}
		return false
	}

	// genesisAddrs prevents synthetic-RNG addresses from colliding with
	// pre-allocated genesis or inject addresses. Random-random
	// collisions across 2^160 addresses are not modelled — astronomical.
	genesisAddrs := make(map[common.Address]struct{},
		len(cfg.GenesisAccounts)+len(cfg.InjectAddresses))

	writeBlob := func(addrHash common.Hash, blob []byte) error {
		if err := batch.Set(addrHash[:], blob, nil); err != nil {
			return err
		}
		pendingBytes += 32 + len(blob)
		if pendingBytes >= phase1FlushBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		checkTarget(len(blob))
		return nil
	}

	// 1a. Genesis-alloc accounts.
	for addr, acc := range cfg.GenesisAccounts {
		genesisAddrs[addr] = struct{}{}
		addrHash := crypto.Keccak256Hash(addr[:])

		var code []byte
		if c, ok := cfg.GenesisCode[addr]; ok {
			code = c
		}
		var slots []entityBlobSlot
		if storage, ok := cfg.GenesisStorage[addr]; ok {
			slots = make([]entityBlobSlot, 0, len(storage))
			for k, v := range storage {
				slots = append(slots, entityBlobSlot{Key: k, Value: v})
			}
			stats.StorageSlotsCreated += len(storage)
		}

		var blob []byte
		if len(code) == 0 && len(slots) == 0 {
			blob = encodeEntityEOA(acc.Nonce, acc.Balance)
			stats.AccountsCreated++
		} else {
			blob = encodeEntityContract(acc.Nonce, acc.Balance, code, slots)
			stats.ContractsCreated++
		}
		if err := writeBlob(addrHash, blob); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase1 genesis alloc: %w", err)
		}
	}

	// 1b. Inject-addresses (e.g. the Anvil default account). Modelled as
	// EOAs with a fixed 999_999_999 ETH balance, matching the
	// generator's existing semantics.
	injectBalance := new(uint256.Int).Mul(uint256.NewInt(999_999_999), uint256.NewInt(1_000_000_000_000_000_000))
	for _, addr := range cfg.InjectAddresses {
		if _, dup := genesisAddrs[addr]; dup {
			continue
		}
		genesisAddrs[addr] = struct{}{}
		addrHash := crypto.Keccak256Hash(addr[:])
		blob := encodeEntityEOA(0, injectBalance)
		if err := writeBlob(addrHash, blob); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase1 inject: %w", err)
		}
		stats.AccountsCreated++
		if cfg.Verbose {
			log.Printf("Injected account %s with %s wei", addr.Hex(), injectBalance.String())
		}
	}

	// 1c. Synthetic EOAs.
	if cfg.LiveStats != nil {
		cfg.LiveStats.SetPhase("accounts")
	}
	for i := 0; i < cfg.NumAccounts && !targetReached; i++ {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, nil, err
		}
		acc := entitygen.GenerateEOA(rng)
		for _, dup := genesisAddrs[acc.Address]; dup; {
			acc = entitygen.GenerateEOA(rng)
			_, dup = genesisAddrs[acc.Address]
		}
		blob := encodeEntityEOA(acc.StateAccount.Nonce, acc.StateAccount.Balance)
		if err := writeBlob(acc.AddrHash, blob); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase1 EOA #%d: %w", i, err)
		}
		stats.AccountsCreated++
		if len(stats.SampleEOAs) < 3 {
			stats.SampleEOAs = append(stats.SampleEOAs, acc.Address)
		}
		if cfg.LiveStats != nil {
			cfg.LiveStats.AddAccount()
		}
	}

	// 1d. Synthetic contracts.
	if cfg.LiveStats != nil {
		cfg.LiveStats.SetPhase("contracts")
	}
	codeSize := cfg.CodeSize
	if codeSize <= 0 {
		codeSize = 1024
	}
	for i := 0; i < cfg.NumContracts && !targetReached; i++ {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, nil, err
		}
		// numSlots is drawn first per entitygen's contract.go RNG
		// contract; this MUST stay before GenerateContract.
		numSlots := entitygen.GenerateSlotCount(rng, cfg.Distribution, cfg.MinSlots, cfg.MaxSlots)
		contract := entitygen.GenerateContract(rng, codeSize, numSlots)
		for _, dup := genesisAddrs[contract.Address]; dup; {
			numSlots = entitygen.GenerateSlotCount(rng, cfg.Distribution, cfg.MinSlots, cfg.MaxSlots)
			contract = entitygen.GenerateContract(rng, codeSize, numSlots)
			_, dup = genesisAddrs[contract.Address]
		}

		// entitygen.Account.Storage is sorted by raw Key; copy into the
		// blob's slice so Phase 2 can re-sort by keccak(Key) later.
		slots := make([]entityBlobSlot, len(contract.Storage))
		for j, s := range contract.Storage {
			slots[j] = entityBlobSlot{Key: s.Key, Value: s.Value}
		}
		blob := encodeEntityContract(
			contract.StateAccount.Nonce,
			contract.StateAccount.Balance,
			contract.Code,
			slots,
		)
		if err := writeBlob(contract.AddrHash, blob); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase1 contract #%d: %w", i, err)
		}
		stats.ContractsCreated++
		stats.StorageSlotsCreated += len(contract.Storage)
		if len(stats.SampleContracts) < 3 {
			stats.SampleContracts = append(stats.SampleContracts, contract.Address)
		}
		if cfg.LiveStats != nil {
			cfg.LiveStats.AddContract(len(contract.Storage))
		}
	}

	if err := flush(); err != nil {
		return common.Hash{}, nil, fmt.Errorf("phase1 final flush: %w", err)
	}
	if err := pdb.Compact(nil, bytes32xFF, false); err != nil {
		return common.Hash{}, nil, fmt.Errorf("phase1 compact: %w", err)
	}

	stats.GenerationTime = time.Since(start)
	if cfg.Verbose {
		log.Printf("[geth MPT Phase 1] complete: %d accounts, %d contracts, %d slots in %v",
			stats.AccountsCreated, stats.ContractsCreated,
			stats.StorageSlotsCreated, stats.GenerationTime.Round(time.Millisecond))
	}

	// --- Phase 2: forward iterate temp Pebble → write production DB. ---

	phase2Start := time.Now()
	if cfg.LiveStats != nil {
		cfg.LiveStats.SetPhase("phase2-trie")
	}

	// Outer account trie. OnTrieNode emits each completed branch/extension
	// node; we persist under PathScheme TrieNodeAccountPrefix.
	var accountCb trie.OnTrieNode
	if cfg.WriteTrieNodes {
		accountCb = func(path []byte, hash common.Hash, blob []byte) {
			// StackTrie warns the path/blob slices are volatile across calls;
			// copy before queuing into the writer's batch.
			p := make([]byte, len(path))
			copy(p, path)
			b := make([]byte, len(blob))
			copy(b, blob)
			key := append([]byte{}, rawdb.TrieNodeAccountPrefix...)
			key = append(key, p...)
			if err := w.PutTrieNode(key, b); err != nil {
				log.Fatalf("write account trie node: %v", err)
			}
		}
	}
	accountTrie := trie.NewStackTrie(accountCb)

	// Code dedup: same hash means same bytes (collision-resistant), so
	// once we've written a particular code blob we don't need to write
	// it again. Bounded by total unique contracts.
	codeSeen := make(map[common.Hash]struct{}, cfg.NumContracts)

	iter, err := pdb.NewIter(nil)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("phase2 open iter: %w", err)
	}
	defer iter.Close()

	count := 0
	for ok := iter.First(); ok; ok = iter.Next() {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, nil, err
		}
		var addrHash common.Hash
		copy(addrHash[:], iter.Key())

		ent, err := decodeEntityBlob(iter.Value())
		if err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase2 decode at #%d: %w", count, err)
		}

		// Build storage trie + collect (sortedSlotHash, encodedValue) for
		// snapshot writes in keccak order.
		storageRoot, sortedSlotEntries, err := buildStorageTrie(w, addrHash, ent.slots, cfg.WriteTrieNodes)
		if err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase2 storage trie at #%d: %w", count, err)
		}

		// Snapshot-side flat state writes (sequential by addrHash, and
		// per-account the slot writes are sequential by slotHash).
		acc := types.StateAccount{
			Nonce:    ent.nonce,
			Balance:  ent.balance,
			Root:     storageRoot,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}
		var codeHash common.Hash
		if len(ent.code) > 0 {
			codeHash = crypto.Keccak256Hash(ent.code)
			acc.CodeHash = codeHash.Bytes()
		}
		if err := w.WriteAccount(common.Address{}, addrHash, &acc, 0); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase2 write account at #%d: %w", count, err)
		}
		for _, s := range sortedSlotEntries {
			if err := w.WriteStorageRLP(addrHash, s.slotHash, s.valueRLP); err != nil {
				return common.Hash{}, nil, fmt.Errorf("phase2 write slot at #%d: %w", count, err)
			}
		}
		if len(ent.code) > 0 {
			if _, dup := codeSeen[codeHash]; !dup {
				if err := w.WriteCode(codeHash, ent.code); err != nil {
					return common.Hash{}, nil, fmt.Errorf("phase2 write code at #%d: %w", count, err)
				}
				codeSeen[codeHash] = struct{}{}
			}
		}

		// Outer account trie input MUST be the FULL StateAccount RLP, not
		// SlimAccountRLP — geth's trie reader decodes leaf values as
		// StateAccount with a fixed 32-byte Root field; SlimAccountRLP
		// elides Root for EOAs and crashes on decode.
		fullRLP, err := rlp.EncodeToBytes(&acc)
		if err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase2 encode account RLP at #%d: %w", count, err)
		}
		if err := accountTrie.Update(addrHash[:], fullRLP); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase2 account trie update at #%d: %w", count, err)
		}

		count++
		if cfg.LiveStats != nil && count%1024 == 0 {
			cfg.LiveStats.SyncBytes(w.Stats())
		}

		// Phase 2 target-size precise stop: every N entities, flush the
		// writer's batch so all queued bytes are on disk, then sample
		// the chaindata directory size. When it reaches cfg.TargetSize
		// we stop iteration with a partial state root that reflects only
		// the entities written so far. The 1024-entity cadence balances
		// sample frequency vs. flush+walkfs overhead — at ~600 B/entity
		// a 1024-batch is ~600 KiB, well below typical target tolerances.
		if cfg.TargetSize > 0 && count%1024 == 0 {
			if err := w.FlushBatch(); err != nil {
				return common.Hash{}, nil, fmt.Errorf("phase2 target-size flush: %w", err)
			}
			if size, err := dirSize(cfg.DBPath); err == nil && size >= cfg.TargetSize {
				if cfg.Verbose {
					log.Printf("geth MPT Phase 2: dirSize %d MiB >= target %d MiB — stopping iteration",
						size>>20, cfg.TargetSize>>20)
				}
				break
			}
		}
	}

	stateRoot := accountTrie.Hash()
	stats.StateRoot = stateRoot
	stats.DBWriteTime = time.Since(phase2Start)
	if cfg.Verbose {
		log.Printf("[geth MPT Phase 2] %d entities → root %s in %v",
			count, stateRoot.Hex(), stats.DBWriteTime.Round(time.Millisecond))
	}

	writerStats := w.Stats()
	stats.AccountBytes = writerStats.AccountBytes
	stats.StorageBytes = writerStats.StorageBytes
	stats.CodeBytes = writerStats.CodeBytes
	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	if cfg.LiveStats != nil {
		cfg.LiveStats.SyncBytes(writerStats)
		cfg.LiveStats.SetStateRoot(stateRoot.Hex())
	}

	return stateRoot, stats, nil
}

// dirSize returns the total bytes used by all regular files under path.
// Used by Phase 2's --target-size sampling: we read the on-disk size of
// the production geth chaindata directory after each batch flush and
// stop iteration once it reaches the requested target. Returns 0 + nil
// if path doesn't exist yet (Pebble may not have created files in the
// first ~ms of operation).
func dirSize(path string) (uint64, error) {
	var total uint64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

// sortedSlot pairs a slot's keccak hash with its RLP-encoded value, used
// by Phase 2 to write storage snapshot entries in keccak order.
type sortedSlot struct {
	slotHash common.Hash
	valueRLP []byte
}

// buildStorageTrie hashes + sorts a contract's storage slots, builds the
// per-account storage StackTrie (emitting trie nodes via the writer when
// cfg.WriteTrieNodes is true), and returns (root, sortedEntries) so the
// caller can write the snapshot in keccak order.
//
// For ≥ parallelKeccakThreshold slots, keccak hashing is parallelised
// across cores — same threshold as the legacy MPT path.
func buildStorageTrie(
	w *Writer,
	accountHash common.Hash,
	slots []entityBlobSlot,
	writeNodes bool,
) (common.Hash, []sortedSlot, error) {
	if len(slots) == 0 {
		return types.EmptyRootHash, nil, nil
	}

	type kv struct {
		Key   common.Hash
		Hash  common.Hash
		Value common.Hash
	}
	hashed := make([]kv, len(slots))
	if len(slots) >= parallelKeccakThreshold {
		numWorkers := runtime.GOMAXPROCS(0)
		chunk := (len(slots) + numWorkers - 1) / numWorkers
		var wg sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			s := w * chunk
			e := s + chunk
			if s >= len(slots) {
				break
			}
			if e > len(slots) {
				e = len(slots)
			}
			wg.Add(1)
			go func(s, e int) {
				defer wg.Done()
				for i := s; i < e; i++ {
					hashed[i] = kv{
						Key:   slots[i].Key,
						Hash:  crypto.Keccak256Hash(slots[i].Key[:]),
						Value: slots[i].Value,
					}
				}
			}(s, e)
		}
		wg.Wait()
	} else {
		for i, s := range slots {
			hashed[i] = kv{
				Key:   s.Key,
				Hash:  crypto.Keccak256Hash(s.Key[:]),
				Value: s.Value,
			}
		}
	}
	sort.Slice(hashed, func(i, j int) bool {
		return bytes.Compare(hashed[i].Hash[:], hashed[j].Hash[:]) < 0
	})

	var storageCb trie.OnTrieNode
	if writeNodes {
		acctHash := accountHash // capture for closure
		storageCb = func(path []byte, hash common.Hash, blob []byte) {
			p := make([]byte, len(path))
			copy(p, path)
			b := make([]byte, len(blob))
			copy(b, blob)
			key := make([]byte, 0, len(rawdb.TrieNodeStoragePrefix)+common.HashLength+len(p))
			key = append(key, rawdb.TrieNodeStoragePrefix...)
			key = append(key, acctHash[:]...)
			key = append(key, p...)
			if err := w.PutTrieNode(key, b); err != nil {
				log.Fatalf("write storage trie node: %v", err)
			}
		}
	}
	storageTrie := trie.NewStackTrie(storageCb)
	out := make([]sortedSlot, 0, len(hashed))
	for _, h := range hashed {
		valueRLP, err := encodeStorageValue(h.Value)
		if err != nil {
			return common.Hash{}, nil, err
		}
		if err := storageTrie.Update(h.Hash[:], valueRLP); err != nil {
			return common.Hash{}, nil, err
		}
		out = append(out, sortedSlot{slotHash: h.Hash, valueRLP: valueRLP})
	}
	return storageTrie.Hash(), out, nil
}
