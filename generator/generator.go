package generator

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	mrand "math/rand"
	"os"
	"runtime"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

// Generator handles state generation.
type Generator struct {
	config Config
	db     ethdb.KeyValueStore // Pebble DB for geth format or temp operations
	writer *GethWriter         // Geth Pebble writer (only format supported)
	rng    *mrand.Rand
}

// New creates a new state generator.
func New(config Config) (*Generator, error) {
	// Validate trie mode
	switch config.TrieMode {
	case TrieModeMPT, TrieModeBinary, "":
		// valid
	default:
		return nil, fmt.Errorf("unsupported trie mode: %q", config.TrieMode)
	}

	// geth is the only output format. OutputFormat is retained on Config
	// as a vestigial field until the follow-up commit removes it.
	config.OutputFormat = OutputGeth

	gethWriter, err := NewGethWriter(config.DBPath, config.BatchSize, config.Workers)
	if err != nil {
		return nil, fmt.Errorf("failed to create geth writer: %w", err)
	}

	return &Generator{
		config: config,
		db:     gethWriter.DB(), // Shared for genesis / trie operations.
		writer: gethWriter,
		rng:    mrand.New(mrand.NewSource(config.Seed)),
	}, nil
}

// Close closes the generator and its database. GethWriter.Close already
// closes the underlying Pebble DB; g.db shares that instance.
func (g *Generator) Close() error {
	if g.writer == nil {
		return nil
	}
	if err := g.writer.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}
	return nil
}

// DB returns the underlying database for external writes (e.g., genesis block).
func (g *Generator) DB() ethdb.KeyValueStore {
	return g.db
}

// Generate generates the state and returns statistics.
// Both MPT and binary trie modes now use streaming approaches to support
// states larger than available RAM.
func (g *Generator) Generate() (*Stats, error) {
	if g.config.TrieMode == TrieModeBinary {
		return g.generateStreamingBinary()
	}
	// MPT mode also uses streaming to support large states
	return g.generateStreamingMPT()
}

// generateStreamingMPT generates state for MPT mode using a fully streaming
// two-phase approach with bounded memory:
//
// Phase 1: Generate entities one at a time (no pre-allocation of all metadata).
//   - For each account/contract: generate data, write snapshots, compute storage
//     root + write storage trie nodes, write (addrHash → slimAccountRLP) to a
//     temp Pebble DB (auto-sorted by addrHash via LSM).
//   - Supports --target-size via periodic dirSize() checks.
//
// Phase 2: Build the account trie from the temp DB.
//   - Iterate temp DB in addrHash order → feed account StackTrie → state root.
//   - O(1) memory — just the StackTrie stack.
//
// Memory: O(max_slots_per_contract) — same as binary trie path.
func (g *Generator) generateStreamingMPT() (*Stats, error) {
	stats := &Stats{}
	start := time.Now()

	// Set up trie node writer for persisting MPT nodes via PathScheme.
	var nodeWriter *mptTrieNodeWriter
	if g.config.WriteTrieNodes && g.db != nil {
		nodeWriter = newMPTTrieNodeWriter(g.db)
		defer nodeWriter.flush()
	}

	// Temp DB for account trie entries: key=addrHash, value=slimAccountRLP.
	// Pebble sorts by key automatically, so Phase 2 iterates in addrHash order.
	tempDir, err := os.MkdirTemp("", "mpt-acct-trie-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	acctTrieDB, err := pebble.New(tempDir, 128, 64, "mpt-acct/", false)
	if err != nil {
		return nil, fmt.Errorf("create temp account trie DB: %w", err)
	}
	defer acctTrieDB.Close()
	acctTrieBatch := acctTrieDB.NewBatch()

	// writeAcctTrieEntry stores an account's trie entry for Phase 2.
	writeAcctTrieEntry := func(addrHash common.Hash, slimData []byte) error {
		if err := acctTrieBatch.Put(addrHash[:], slimData); err != nil {
			return err
		}
		if acctTrieBatch.ValueSize() >= 64*1024*1024 {
			if err := acctTrieBatch.Write(); err != nil {
				return err
			}
			acctTrieBatch.Reset()
		}
		return nil
	}

	// Genesis addresses set — only for collision avoidance with genesis accounts.
	// Random-random collisions are astronomically unlikely (~2^-80) and not checked.
	genesisAddrs := make(map[common.Address]bool, len(g.config.GenesisAccounts))

	// mptStorageEntry holds a pre-computed storage snapshot entry for async writing.
	type mptStorageEntry struct {
		addrHash common.Hash
		slotHash common.Hash
		valueRLP []byte
		// For deep-branch phantom entries (WriteRawStorage path):
		isRaw   bool
		rawAddr common.Address
		rawSlot common.Hash
		rawVal  common.Hash
	}

	// Async snapshot writing: ALL snapshot writes (storage + account + code)
	// are done by a background goroutine, so the main goroutine only does
	// trie computation. This overlaps I/O with the storage root computation
	// of the next contract.
	type accountSnapWork struct {
		addr           common.Address
		addrHash       common.Hash
		acc            types.StateAccount // value copy (avoid race)
		code           []byte
		codeHash       common.Hash
		storageEntries []mptStorageEntry // pre-computed storage snapshots
	}

	snapCh := make(chan accountSnapWork, 64)
	snapErrCh := make(chan error, 1)
	var snapWg sync.WaitGroup
	snapWg.Add(1)
	go func() {
		defer snapWg.Done()
		for work := range snapCh {
			// Write storage snapshots
			for _, s := range work.storageEntries {
				if s.isRaw {
					if err := g.writer.WriteRawStorage(s.rawAddr, 0, s.rawSlot, s.rawVal); err != nil {
						snapErrCh <- fmt.Errorf("write raw storage: %w", err)
						return
					}
				} else if s.valueRLP != nil {
					if err := g.writer.WriteStorageRLP(s.addrHash, s.slotHash, s.valueRLP); err != nil {
						snapErrCh <- fmt.Errorf("write storage: %w", err)
						return
					}
				} else {
					// Non-phantom deep-branch entry with pre-hashed key
					if err := g.writer.WriteStorage(s.rawAddr, s.addrHash, s.rawSlot, s.slotHash, s.rawVal); err != nil {
						snapErrCh <- fmt.Errorf("write storage: %w", err)
						return
					}
				}
			}
			// Write code
			if len(work.code) > 0 {
				if err := g.writer.WriteCode(work.codeHash, work.code); err != nil {
					snapErrCh <- fmt.Errorf("write code: %w", err)
					return
				}
			}
			// Write account snapshot
			if err := g.writer.WriteAccount(work.addr, work.addrHash, &work.acc, 0); err != nil {
				snapErrCh <- fmt.Errorf("write account: %w", err)
				return
			}
			// Store trie entry for Phase 2.
			// MUST use full StateAccount RLP (not SlimAccountRLP) because geth's
			// trie reader decodes leaf values as StateAccount with a fixed 32-byte
			// Root field. SlimAccountRLP omits Root for EOAs → decode crash:
			// "rlp: input string too short for common.Hash"
			trieData, err := rlp.EncodeToBytes(&work.acc)
			if err != nil {
				snapErrCh <- fmt.Errorf("encode account for trie: %w", err)
				return
			}
			if err := writeAcctTrieEntry(work.addrHash, trieData); err != nil {
				snapErrCh <- fmt.Errorf("write trie entry: %w", err)
				return
			}
		}
	}()

	// sendSnapshot sends all snapshot work to the background goroutine.
	sendSnapshot := func(addr common.Address, addrHash common.Hash, acc *types.StateAccount, code []byte, codeHash common.Hash, storageEntries []mptStorageEntry) error {
		select {
		case err := <-snapErrCh:
			return err
		default:
		}
		snapCh <- accountSnapWork{
			addr:           addr,
			addrHash:       addrHash,
			acc:            *acc,
			code:           code,
			codeHash:       codeHash,
			storageEntries: storageEntries,
		}
		return nil
	}

	// processAccount sends an account with no storage entries.
	processAccount := func(addr common.Address, acc *types.StateAccount, code []byte, codeHash common.Hash) error {
		addrHash := crypto.Keccak256Hash(addr[:])
		return sendSnapshot(addr, addrHash, acc, code, codeHash, nil)
	}

	// mptKeyWithHash pairs a storage slot with its keccak256 hash for MPT sorting.
	type mptKeyWithHash struct {
		slot    storageSlot
		keyHash common.Hash
	}

	// parallelKeccakThreshold: use parallel hashing for contracts with >= 64 slots.
	// Same threshold as binary trie's collectStorageEntriesParallel.
	const parallelKeccakThreshold = 64

	// computeStorageRootMPT generates storage slots, computes the storage trie
	// root, and returns collected storage entries for async snapshot writing.
	// Does NOT write snapshots — those are sent to the background goroutine.
	// For contracts with >= 64 slots, keccak hashing is parallelized across cores.
	computeStorageRootMPT := func(addr common.Address, addrHash common.Hash, numSlots int, rng *mrand.Rand) (common.Hash, []mptStorageEntry, error) {
		if numSlots == 0 {
			return types.EmptyRootHash, nil, nil
		}

		var storageCb trie.OnTrieNode
		if nodeWriter != nil {
			storageCb = nodeWriter.storageCallback(addrHash)
		}
		storageTrie := trie.NewStackTrie(storageCb)

		// Step 1: Generate slots from RNG (must be sequential for determinism).
		slots := make([]storageSlot, numSlots)
		for j := 0; j < numSlots; j++ {
			rng.Read(slots[j].Key[:])
			rng.Read(slots[j].Value[:])
			if slots[j].Value == (common.Hash{}) {
				slots[j].Value[31] = 1
			}
		}

		// Step 2: Hash keys — parallel for large contracts, sequential for small.
		withHashes := make([]mptKeyWithHash, numSlots)
		if numSlots >= parallelKeccakThreshold {
			numWorkers := runtime.GOMAXPROCS(0)
			chunkSize := (numSlots + numWorkers - 1) / numWorkers
			var wg sync.WaitGroup
			for w := 0; w < numWorkers; w++ {
				s := w * chunkSize
				e := min(s+chunkSize, numSlots)
				if s >= numSlots {
					break
				}
				wg.Add(1)
				go func(start, end int) {
					defer wg.Done()
					for i := start; i < end; i++ {
						withHashes[i] = mptKeyWithHash{
							slot:    slots[i],
							keyHash: crypto.Keccak256Hash(slots[i].Key[:]),
						}
					}
				}(s, e)
			}
			wg.Wait()
		} else {
			for i := range slots {
				withHashes[i] = mptKeyWithHash{
					slot:    slots[i],
					keyHash: crypto.Keccak256Hash(slots[i].Key[:]),
				}
			}
		}

		// Step 3: Sort by keccak256(key) for MPT StackTrie ordering.
		sort.Slice(withHashes, func(i, j int) bool {
			return bytes.Compare(withHashes[i].keyHash[:], withHashes[j].keyHash[:]) < 0
		})

		// Step 4: Feed StackTrie + collect storage entries for async snapshot writing.
		storageEntries := make([]mptStorageEntry, 0, numSlots)
		for _, kh := range withHashes {
			valueRLP, err := encodeStorageValue(kh.slot.Value)
			if err != nil {
				return common.Hash{}, nil, err
			}
			storageEntries = append(storageEntries, mptStorageEntry{
				addrHash: addrHash,
				slotHash: kh.keyHash,
				valueRLP: valueRLP,
			})
			storageTrie.Update(kh.keyHash[:], valueRLP)
		}

		return storageTrie.Hash(), storageEntries, nil
	}

	var lastLogTime = time.Now()
	logProgress := func(phase string, count int) {
		if g.config.Verbose && time.Since(lastLogTime) > 20*time.Second {
			lastLogTime = time.Now()
			log.Printf("[MPT Phase 1] %s: %d processed", phase, count)
		}
	}

	// ============================================================
	// Phase 1a: Genesis accounts
	// ============================================================
	genesisEOAs, genesisContracts := 0, 0
	for addr, acc := range g.config.GenesisAccounts {
		genesisAddrs[addr] = true
		addrHash := crypto.Keccak256Hash(addr[:])

		stateAccount := *acc
		var code []byte
		var codeHash common.Hash

		if gCode, ok := g.config.GenesisCode[addr]; ok {
			code = gCode
			codeHash = crypto.Keccak256Hash(code)
			genesisContracts++
		}

		// Genesis storage
		if gStorage, ok := g.config.GenesisStorage[addr]; ok {
			// Compute storage root
			var storageCb trie.OnTrieNode
			if nodeWriter != nil {
				storageCb = nodeWriter.storageCallback(addrHash)
			}
			storageTrie := trie.NewStackTrie(storageCb)

			slots := mapToSortedSlots(gStorage)
			// Re-sort by keccak for MPT
			type keyWithHash struct {
				slot    storageSlot
				keyHash common.Hash
			}
			withHashes := make([]keyWithHash, len(slots))
			for i, slot := range slots {
				withHashes[i] = keyWithHash{slot: slot, keyHash: crypto.Keccak256Hash(slot.Key[:])}
			}
			sort.Slice(withHashes, func(i, j int) bool {
				return bytes.Compare(withHashes[i].keyHash[:], withHashes[j].keyHash[:]) < 0
			})
			genesisStorageEntries := make([]mptStorageEntry, 0, len(withHashes))
			for _, kh := range withHashes {
				valueRLP, err := encodeStorageValue(kh.slot.Value)
				if err != nil {
					return nil, err
				}
				genesisStorageEntries = append(genesisStorageEntries, mptStorageEntry{
					addrHash: addrHash,
					slotHash: kh.keyHash,
					valueRLP: valueRLP,
				})
				storageTrie.Update(kh.keyHash[:], valueRLP)
			}
			stateAccount.Root = storageTrie.Hash()
			stats.StorageSlotsCreated += len(gStorage)
			if code == nil {
				genesisContracts++
			}
			if err := sendSnapshot(addr, addrHash, &stateAccount, code, codeHash, genesisStorageEntries); err != nil {
				return nil, fmt.Errorf("send genesis account snapshot: %w", err)
			}
		} else {
			if code == nil {
				genesisEOAs++
			}
			if err := processAccount(addr, &stateAccount, code, codeHash); err != nil {
				return nil, fmt.Errorf("process genesis account: %w", err)
			}
		}
	}
	stats.AccountsCreated = genesisEOAs
	stats.ContractsCreated = genesisContracts

	if g.config.Verbose && len(g.config.GenesisAccounts) > 0 {
		log.Printf("Included %d genesis alloc accounts (%d EOAs, %d contracts)",
			len(g.config.GenesisAccounts), genesisEOAs, genesisContracts)
	}

	// ============================================================
	// Phase 1b: EOAs (streaming, one at a time)
	// ============================================================
	if g.config.LiveStats != nil {
		g.config.LiveStats.SetPhase("accounts")
	}

	for i := 0; i < g.config.NumAccounts; i++ {
		var addr common.Address
		g.rng.Read(addr[:])
		for genesisAddrs[addr] {
			g.rng.Read(addr[:])
		}

		nonce := uint64(g.rng.Intn(1000))
		balance := new(uint256.Int).Mul(uint256.NewInt(uint64(g.rng.Intn(1000))), uint256.NewInt(1e18))

		acc := &types.StateAccount{
			Nonce:    nonce,
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}

		if err := processAccount(addr, acc, nil, common.Hash{}); err != nil {
			return nil, fmt.Errorf("process EOA %d: %w", i, err)
		}

		stats.AccountsCreated++
		if len(stats.SampleEOAs) < 3 {
			stats.SampleEOAs = append(stats.SampleEOAs, addr)
		}

		if g.config.LiveStats != nil {
			g.config.LiveStats.AddAccount()
		}
		logProgress("EOAs", stats.AccountsCreated)
	}

	// ============================================================
	// Phase 1c: Contracts (streaming, one at a time)
	// ============================================================
	if g.config.LiveStats != nil {
		g.config.LiveStats.SetPhase("contracts")
	}

	targetReached := false
	contractIdx := 0

	for contractIdx < g.config.NumContracts {
		var addr common.Address
		g.rng.Read(addr[:])
		for genesisAddrs[addr] {
			g.rng.Read(addr[:])
		}

		// RNG sequence must match old generateAccountMetas:
		// Intn(1000) for nonce, Intn(100) for balance, Intn(CodeSize) for codeSize, Int63() for codeSeed
		nonce := uint64(g.rng.Intn(1000))
		balance := new(uint256.Int).Mul(uint256.NewInt(uint64(g.rng.Intn(100))), uint256.NewInt(1e18))
		codeSize := g.config.CodeSize + g.rng.Intn(g.config.CodeSize)
		codeSeed := g.rng.Int63()
		numSlots := g.generateSlotCount()

		// Generate code from seed
		rng := mrand.New(mrand.NewSource(codeSeed))
		code := make([]byte, codeSize)
		rng.Read(code)
		codeHash := crypto.Keccak256Hash(code)

		// Compute storage root (trie nodes written inline, snapshots collected for async).
		addrHash := crypto.Keccak256Hash(addr[:])
		storageRoot, storageEntries, err := computeStorageRootMPT(addr, addrHash, numSlots, rng)
		if err != nil {
			return nil, fmt.Errorf("compute storage root for contract %d: %w", contractIdx, err)
		}

		acc := &types.StateAccount{
			Nonce:    nonce,
			Balance:  balance,
			Root:     storageRoot,
			CodeHash: codeHash.Bytes(),
		}

		// Send all snapshot writes (storage + account + code) to background goroutine.
		if err := sendSnapshot(addr, addrHash, acc, code, codeHash, storageEntries); err != nil {
			return nil, fmt.Errorf("send snapshot for contract %d: %w", contractIdx, err)
		}

		stats.ContractsCreated++
		stats.StorageSlotsCreated += numSlots
		if len(stats.SampleContracts) < 3 {
			stats.SampleContracts = append(stats.SampleContracts, addr)
		}

		if g.config.LiveStats != nil {
			g.config.LiveStats.AddContract(numSlots)
			if stats.ContractsCreated%100 == 0 {
				g.config.LiveStats.SyncBytes(g.writer.Stats())
			}
		}

		contractIdx++
		logProgress("contracts", contractIdx)

		// Factor-free target-size stop for MPT: direct dirSize measurement
		// every 500 contracts. Unlike bintrie (where most data lands only
		// in Phase 2 after temp-DB transformation), MPT's flat state,
		// storage trie nodes, and code blobs all hit main DB synchronously
		// in Phase 1 — so dirSize is the authoritative answer.
		//
		// We flush three things before sampling disk:
		//   1. nodeWriter's owned batch (synchronous, single-goroutine)
		//   2. The writer's currently-buffered batch (non-shutdown partial
		//      flush) so flat-state/code writes land on disk before dirSize
		//      walks the filesystem — otherwise we'd under-sample by up to
		//      BatchSize × item-size.
		// The in-flight snapCh queue (≤64 items ≈ ~0.5 MB) remains unseen
		// but is bounded and small relative to any practical target.
		if g.config.TargetSize > 0 && contractIdx%500 == 0 {
			if nodeWriter != nil {
				nodeWriter.flush()
			}
			if err := g.writer.FlushBatch(); err != nil {
				return nil, fmt.Errorf("MPT target-size flush: %w", err)
			}
			ms, err := dirSize(g.config.DBPath)
			if err == nil && ms >= g.config.TargetSize {
				if g.config.Verbose {
					log.Printf("Target size reached: %s on disk (target: %s)",
						formatBytesInternal(ms),
						formatBytesInternal(g.config.TargetSize))
				}
				targetReached = true
				break
			}
		}
	}

	// Deep-branch contracts
	if g.config.DeepBranch.Enabled() {
		deepSlots := g.config.DeepBranch.KnownSlots * (1 + g.config.DeepBranch.Depth)
		for i := 0; i < g.config.DeepBranch.NumAccounts; i++ {
			var addr common.Address
			g.rng.Read(addr[:])
			for genesisAddrs[addr] {
				g.rng.Read(addr[:])
			}

			balance := new(uint256.Int).Mul(uint256.NewInt(uint64(g.rng.Intn(100)+1)), uint256.NewInt(1e18))
			codeSize := g.config.CodeSize + g.rng.Intn(g.config.CodeSize)
			codeSeed := g.rng.Int63()

			rng := mrand.New(mrand.NewSource(codeSeed))
			code := make([]byte, codeSize)
			rng.Read(code)
			codeHash := crypto.Keccak256Hash(code)
			addrHash := crypto.Keccak256Hash(addr[:])

			entries := generateDeepBranchStorage(
				g.config.DeepBranch.KnownSlots,
				g.config.DeepBranch.Depth,
				rng,
			)

			var deepStorageCb trie.OnTrieNode
			if nodeWriter != nil {
				deepStorageCb = nodeWriter.storageCallback(addrHash)
			}
			storageTrie := trie.NewStackTrie(deepStorageCb)
			deepEntries := make([]mptStorageEntry, 0, len(entries))
			for _, entry := range entries {
				if entry.isPhantom {
					deepEntries = append(deepEntries, mptStorageEntry{
						isRaw: true, rawAddr: addr, rawSlot: entry.trieKey, rawVal: entry.value,
					})
				} else {
					slotHash := crypto.Keccak256Hash(entry.rawSlotKey[:])
					deepEntries = append(deepEntries, mptStorageEntry{
						rawAddr: addr, addrHash: addrHash, rawSlot: entry.rawSlotKey, slotHash: slotHash, rawVal: entry.value,
					})
				}
				valueRLP, err := encodeStorageValue(entry.value)
				if err != nil {
					return nil, err
				}
				storageTrie.Update(entry.trieKey[:], valueRLP)
			}

			acc := &types.StateAccount{
				Nonce:    1,
				Balance:  balance,
				Root:     storageTrie.Hash(),
				CodeHash: codeHash.Bytes(),
			}
			if err := sendSnapshot(addr, addrHash, acc, code, codeHash, deepEntries); err != nil {
				return nil, fmt.Errorf("send deep-branch snapshot: %w", err)
			}

			stats.StorageSlotsCreated += deepSlots
			stats.ContractsCreated++
		}
		stats.DeepBranchAccounts = g.config.DeepBranch.NumAccounts
		stats.DeepBranchDepth = g.config.DeepBranch.Depth

		if g.config.Verbose {
			log.Printf("Added %d deep-branch contracts (depth=%d, known_slots=%d)",
				g.config.DeepBranch.NumAccounts, g.config.DeepBranch.Depth,
				g.config.DeepBranch.KnownSlots)
		}
	}

	stats.GenerationTime = time.Since(start)

	if g.config.Verbose {
		log.Printf("[MPT Phase 1] complete: %d accounts, %d contracts, %d slots",
			stats.AccountsCreated, stats.ContractsCreated, stats.StorageSlotsCreated)
		if targetReached {
			log.Printf("[MPT Phase 1] stopped early — target size reached")
		}
	}

	// Wait for async snapshot writer to finish.
	close(snapCh)
	snapWg.Wait()
	select {
	case err := <-snapErrCh:
		return nil, fmt.Errorf("snapshot writer: %w", err)
	default:
	}

	// Flush Phase 1 writes.
	if acctTrieBatch.ValueSize() > 0 {
		if err := acctTrieBatch.Write(); err != nil {
			return nil, fmt.Errorf("flush account trie batch: %w", err)
		}
	}
	if err := g.writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush snapshot writes: %w", err)
	}

	// ============================================================
	// Phase 2: Build account trie from temp DB (sorted by addrHash)
	// ============================================================

	// Compact temp DB to flatten LSM levels for single-pass sequential iteration.
	// Same optimization the binary trie path uses before Phase 2.
	if err := acctTrieDB.Compact(nil, nil); err != nil {
		return nil, fmt.Errorf("compact account trie temp DB: %w", err)
	}

	phase2Start := time.Now()

	var acctCallback trie.OnTrieNode
	if nodeWriter != nil {
		acctCallback = nodeWriter.accountCallback()
	}
	accountTrie := trie.NewStackTrie(acctCallback)

	// Phase 2 runs to completion: for MPT, the account trie is small (~5%
	// of final DB at GB scale) so letting it finish adds a bounded amount
	// beyond target. Stopping mid-Phase-2 with an iterator wrapper would
	// produce a state root that doesn't correspond to any complete subset
	// of the written flat accounts — unlike bintrie where Phase 2 discards
	// entries from the temp DB, MPT Phase 2 reads from acctTrieDB written
	// lockstep with the flat-state writes the user already sees on disk.
	iter := acctTrieDB.NewIterator(nil, nil)
	acctTrieCount := 0
	for iter.Next() {
		accountTrie.Update(iter.Key(), iter.Value())
		acctTrieCount++
	}
	iter.Release()

	stateRoot := accountTrie.Hash()
	stats.StateRoot = stateRoot

	if err := g.writer.SetStateRoot(stateRoot); err != nil {
		return nil, fmt.Errorf("failed to write state root: %w", err)
	}

	if g.config.Verbose {
		log.Printf("[MPT Phase 2] account trie built from %d entries in %v",
			acctTrieCount, time.Since(phase2Start))
		log.Printf("State root: %s", stateRoot.Hex())
	}

	stats.DBWriteTime = time.Since(start) - stats.GenerationTime

	writerStats := g.writer.Stats()
	stats.AccountBytes = writerStats.AccountBytes
	stats.StorageBytes = writerStats.StorageBytes
	stats.CodeBytes = writerStats.CodeBytes

	if nodeWriter != nil {
		nodes, nbytes := nodeWriter.stats()
		stats.TrieNodeBytes = uint64(nbytes)
		if g.config.Verbose {
			log.Printf("MPT trie nodes written: %d nodes, %s", nodes, formatBytesInternal(uint64(nbytes)))
		}
	}
	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes + stats.TrieNodeBytes

	return stats, nil
}

// generateStreamingBinary generates state for binary trie mode using a
// two-phase approach:
//
// Phase 1: Generate account/contract/storage data, write snapshot entries to
// Pebble (via batchWriter), and collect trie entries (key-value pairs) into a
// flat in-memory slice. Each entry is 64 bytes (32-byte key + 32-byte value).
//
// Phase 2: Sort entries by key, then compute the binary trie root hash via
// recursive divide-and-conquer — grouping by stem, computing StemNode hashes,
// and building the InternalNode tree. No BinaryTrie object, no disk I/O for
// trie nodes, no commit/reopen cycles.
//
// This approach is analogous to how MPT mode uses StackTrie: sorted input
// enables streaming construction. It eliminates the 35x penalty from
// HashedNode disk resolution that plagued the old commit-interval approach.
//
// Memory: O(N × 64 bytes) for the entries slice, where N is the total number
// of trie entries (accounts × 2 + storage slots + code chunks).
func (g *Generator) generateStreamingBinary() (retStats *Stats, retErr error) {
	stats := &Stats{}
	start := time.Now()

	if g.config.CommitInterval > 0 && g.config.Verbose {
		log.Printf("NOTE: --commit-interval is ignored (binary stack trie computes root from sorted entries)")
	}

	// Note: We use g.writer (StateWriter) for final output, not batchWriter

	// Background goroutine writes code entries ("c" + keccak256(code)) during
	// Phase 1. Account/storage flat state is NOT written here — stem blobs are
	// written in Phase 2 by the builder goroutine via stemBlobWriter.
	type snapshotWork struct {
		acc *accountData
	}
	snapCh := make(chan snapshotWork, 64)
	var snapErr atomic.Value // stores error
	var snapWg sync.WaitGroup
	snapWg.Add(1)
	go func() {
		defer snapWg.Done()
		for sw := range snapCh {
			if err := g.writeCodeOnly(sw.acc); err != nil {
				snapErr.Store(err)
				for range snapCh {
				}
				return
			}
		}
	}()
	var snapCloseOnce sync.Once
	closeSnap := func() {
		snapCloseOnce.Do(func() { close(snapCh) })
	}
	defer func() {
		closeSnap()
		snapWg.Wait()
		if e := snapErr.Load(); e != nil && retErr == nil {
			retErr = e.(error)
		}
	}()

	// --- Phase 1: Generate data, write snapshots, write trie entries to temp DB ---
	//
	// Instead of collecting entries in an in-memory slice (which grows linearly
	// with state size), we write each trie entry to a temporary Pebble DB.
	// Pebble's LSM tree keeps keys sorted automatically, so Phase 2 can
	// iterate in order without an explicit sort step. Memory stays O(1).
	tempDir, err := os.MkdirTemp("", "state-actor-sort-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	tempDB, err := pebble.New(tempDir, 128, 64, "temp/", false)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp sort DB: %w", err)
	}
	defer tempDB.Close()

	tempBatch := tempDB.NewBatch()

	// Entry counters are split so future target-size logic can distinguish
	// preamble entries (genesis alloc, inject-addresses, EOA loops) from
	// contract-loop-generated entries. Progress logs and Phase-2 diagnostics
	// use the sum (totalEntries).
	var preambleEntries, contractEntries int64

	// writeEntries writes a batch of trie entries to the temp DB, incrementing
	// the caller-supplied counter (either &preambleEntries or &contractEntries).
	writeEntries := func(entries []trieEntry, counter *int64) error {
		for i := range entries {
			if err := tempBatch.Put(entries[i].Key[:], entries[i].Value[:]); err != nil {
				return err
			}
			*counter++
			if tempBatch.ValueSize() >= 64*1024*1024 { // flush every 64 MB
				if err := tempBatch.Write(); err != nil {
					return err
				}
				tempBatch.Reset()
			}
		}
		return nil
	}

	var lastLogTime = time.Now()
	logProgress := func(phase string, current, total int, slots int64) {
		if time.Since(lastLogTime) < 20*time.Second {
			return
		}
		lastLogTime = time.Now()
		totalEntries := preambleEntries + contractEntries
		pct := float64(current) / float64(total) * 100
		log.Printf("[%s] %d/%d (%.1f%%), %d storage slots, %d trie entries",
			phase, current, total, pct, slots, totalEntries)
	}

	// Track genesis addresses for collision avoidance.
	genesisAddrs := make(map[common.Address]bool, len(g.config.GenesisAccounts))

	// Reusable entries buffer — collectAccountEntries appends to this,
	// then writeEntries drains it, then we reset to reuse the backing array.
	var entryBuf []trieEntry

	// 1a. Genesis alloc accounts.
	for addr, acc := range g.config.GenesisAccounts {
		genesisAddrs[addr] = true

		addrHash := crypto.Keccak256Hash(addr[:])
		codeHash := common.BytesToHash(acc.CodeHash)

		ad := &accountData{
			address:  addr,
			addrHash: addrHash,
			account:  acc,
		}
		if storageMap, ok := g.config.GenesisStorage[addr]; ok {
			ad.storage = mapToSortedSlots(storageMap)
			stats.StorageSlotsCreated += len(ad.storage)
		}
		if code, ok := g.config.GenesisCode[addr]; ok {
			ad.code = code
			ad.codeHash = codeHash
		}

		entryBuf = collectAccountEntries(addr, acc, len(ad.code), ad.code, ad.storage, entryBuf[:0])
		if err := writeEntries(entryBuf, &preambleEntries); err != nil {
			return nil, fmt.Errorf("failed to write genesis trie entries: %w", err)
		}
		snapCh <- snapshotWork{acc: ad}

		if len(ad.code) > 0 || len(ad.storage) > 0 {
			stats.ContractsCreated++
		} else {
			stats.AccountsCreated++
		}
	}

	if g.config.Verbose && len(g.config.GenesisAccounts) > 0 {
		log.Printf("Included %d genesis alloc accounts (%d EOAs, %d contracts)",
			len(g.config.GenesisAccounts), stats.AccountsCreated, stats.ContractsCreated)
	}

	// Inject any explicitly-requested addresses (e.g. Anvil's default account).
	for _, addr := range g.config.InjectAddresses {
		if genesisAddrs[addr] {
			continue
		}
		genesisAddrs[addr] = true
		injectBalance := new(uint256.Int).Mul(uint256.NewInt(999999999), uint256.NewInt(1e18))
		injectAccount := &types.StateAccount{
			Nonce:    0,
			Balance:  injectBalance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		}
		entryBuf = collectAccountEntries(addr, injectAccount, 0, nil, nil, entryBuf[:0])
		if err := writeEntries(entryBuf, &preambleEntries); err != nil {
			return nil, fmt.Errorf("failed to write injected trie entries: %w", err)
		}
		ad := &accountData{
			address:  addr,
			addrHash: crypto.Keccak256Hash(addr[:]),
			account:  injectAccount,
		}
		snapCh <- snapshotWork{acc: ad}
		stats.AccountsCreated++
		if g.config.Verbose {
			log.Printf("Injected account %s with %s wei", addr.Hex(), injectBalance.String())
		}
	}

	// 1b. EOA generation.
	if g.config.LiveStats != nil {
		g.config.LiveStats.SetPhase("accounts")
	}
	for i := 0; i < g.config.NumAccounts; i++ {
		acc := g.generateEOA()
		for genesisAddrs[acc.address] {
			acc = g.generateEOA()
		}

		entryBuf = collectAccountEntries(acc.address, acc.account, 0, nil, nil, entryBuf[:0])
		if err := writeEntries(entryBuf, &preambleEntries); err != nil {
			return nil, fmt.Errorf("failed to write EOA trie entries: %w", err)
		}
		snapCh <- snapshotWork{acc: acc}
		stats.AccountsCreated++
		if g.config.LiveStats != nil {
			g.config.LiveStats.AddAccount()
			// Sync byte stats every 1000 accounts
			if stats.AccountsCreated%1000 == 0 {
				g.config.LiveStats.SyncBytes(g.writer.Stats())
			}
		}
		if len(stats.SampleEOAs) < 3 {
			stats.SampleEOAs = append(stats.SampleEOAs, acc.address)
		}
		logProgress("EOA", i+1, g.config.NumAccounts, 0)
	}

	// 1c. Contract generation via producer-consumer pipeline.
	// When --target-size governs, slot counts are generated on-demand
	// (one RNG call per contract, same sequence as generateSlotDistribution).
	// When --contracts governs, we pre-compute the distribution for the
	// known count.
	// Pre-compute slot distribution only for moderate contract counts.
	// Above 10M, the []int array alone costs 80MB+, so use on-demand generation.
	const maxPrecomputeContracts = 10_000_000
	var slotDistribution []int
	if g.config.NumContracts <= maxPrecomputeContracts {
		slotDistribution = g.generateSlotDistribution()
	}

	done := make(chan struct{})
	contractCh := make(chan *accountData, 16)
	go func() {
		defer close(contractCh)
		for i := 0; i < g.config.NumContracts; i++ {
			var numSlots int
			if slotDistribution != nil {
				numSlots = slotDistribution[i]
			} else {
				numSlots = g.generateSlotCount()
			}
			contract := g.generateContract(numSlots)
			for genesisAddrs[contract.address] {
				contract = g.generateContract(numSlots)
			}
			select {
			case contractCh <- contract:
			case <-done:
				return
			}
		}
	}()

	if g.config.LiveStats != nil {
		g.config.LiveStats.SetPhase("contracts")
	}
	contractIdx := 0
	targetReached := false
	for contract := range contractCh {
		var entries []trieEntry
		if len(contract.storage) >= parallelStorageThreshold {
			entries = collectAccountEntriesParallel(contract.address, contract.account, len(contract.code), contract.code, contract.storage)
		} else {
			entryBuf = collectAccountEntries(contract.address, contract.account, len(contract.code), contract.code, contract.storage, entryBuf[:0])
			entries = entryBuf
		}
		if err := writeEntries(entries, &contractEntries); err != nil {
			return nil, fmt.Errorf("failed to write contract trie entries: %w", err)
		}
		snapCh <- snapshotWork{acc: contract}
		stats.ContractsCreated++
		stats.StorageSlotsCreated += len(contract.storage)
		if g.config.LiveStats != nil {
			g.config.LiveStats.AddContract(len(contract.storage))
			// Sync byte stats every 100 contracts
			if stats.ContractsCreated%100 == 0 {
				g.config.LiveStats.SyncBytes(g.writer.Stats())
			}
		}
		if len(stats.SampleContracts) < 3 {
			stats.SampleContracts = append(stats.SampleContracts, contract.address)
		}
		contractIdx++
		logProgress("Contract", contractIdx, g.config.NumContracts, int64(stats.StorageSlotsCreated))

		// Phase 1 raw-byte safety cap: stop generating more entries once
		// the temp DB's raw bytes (each entry is 32B key + 32B value = 64B)
		// reach TargetSize. This prevents Phase 1 from producing more raw
		// data than target can hold on disk after Phase 2 transformation.
		// The fine-grained factor-free stop runs in Phase 2 via the
		// SizeTracker; this cap keeps Phase 1 bounded for workloads where
		// NumContracts is set generously (test fixtures, or main.go's
		// userContracts×5 safety cap). No compression-ratio assumption —
		// the cap is "never write more raw entries than the target can
		// hold if it were a 1:1 mapping".
		rawBytes := 64 * (preambleEntries + contractEntries)
		if g.config.TargetSize > 0 && uint64(rawBytes) >= g.config.TargetSize {
			if g.config.Verbose {
				log.Printf("Phase 1 raw-byte cap: %s raw entries >= target %s — Phase 2 will trim to target",
					formatBytesInternal(uint64(rawBytes)),
					formatBytesInternal(g.config.TargetSize))
			}
			targetReached = true
			close(done)
			break
		}
	}
	// Drain producer if we broke early.
	if targetReached {
		for range contractCh {
		}
	}

	// Flush remaining temp entries.
	if tempBatch.ValueSize() > 0 {
		if err := tempBatch.Write(); err != nil {
			return nil, fmt.Errorf("failed to flush temp batch: %w", err)
		}
	}

	// --- Phase 2: Stream sorted entries from temp DB → compute root hash ---

	totalEntries := preambleEntries + contractEntries

	// Compact the temp DB to flatten LSM levels into a single sorted run.
	// This makes the sequential iteration single-pass I/O instead of a
	// multi-level merge, reducing per-key CPU overhead across billions of entries.
	if g.config.Verbose {
		log.Printf("Compacting temp DB (%d entries)...", totalEntries)
	}
	compactStart := time.Now()
	if err := tempDB.Compact(nil, nil); err != nil {
		return nil, fmt.Errorf("failed to compact temp DB: %w", err)
	}
	if g.config.Verbose {
		log.Printf("Temp DB compaction complete in %v", time.Since(compactStart).Round(time.Millisecond))
	}

	if g.config.Verbose {
		log.Printf("Computing root from %d trie entries (streaming, O(depth) memory)...", totalEntries)
	}

	hashStart := time.Now()

	// Hoist trieNodeWriter + stemBlobWriter construction here so the
	// SizeTracker can reference their live byte counters, and the
	// stoppableIterator + afterStem callback can plug into the Phase 2
	// pipeline (C4 of the factor-free target-size refactor).
	var tnw *trieNodeWriter
	if g.config.WriteTrieNodes && g.config.OutputFormat == OutputGeth && g.db != nil {
		tnw = &trieNodeWriter{batch: g.db.NewBatch(), db: g.db}
	}
	var sbw *stemBlobWriter
	if g.config.OutputFormat == OutputGeth && g.db != nil {
		sbw = &stemBlobWriter{batch: g.db.NewBatch(), db: g.db}
	}

	// Build the SizeTracker + stoppableIterator when --target-size is set.
	// Logical bytes = stem-blob writer + trie-node writer + Phase-1 code blobs.
	// Preamble/contract entry data is not yet on disk in main DB — it lives
	// in the temp DB and only becomes main-DB bytes via stem blobs and trie
	// nodes emitted here in Phase 2.
	var tracker *SizeTracker
	if g.config.TargetSize > 0 && g.config.OutputFormat == OutputGeth && g.db != nil {
		tracker = NewSizeTracker(g.config.DBPath, g.config.TargetSize, func() int64 {
			var logical int64
			if sbw != nil {
				logical += sbw.Bytes()
			}
			if tnw != nil {
				logical += tnw.Bytes()
			}
			logical += int64(g.writer.Stats().CodeBytes)
			return logical
		})
	}

	var iter ethdb.Iterator = tempDB.NewIterator(nil, nil)
	if tracker != nil {
		iter = &stoppableIterator{
			Iterator:   iter,
			shouldStop: tracker.ShouldStop,
		}
	}

	// afterStem runs in the builder goroutine (the goroutine that owns the
	// sbw + tnw Pebble batches) so MaybeCalibrate's synchronous flush
	// respects Pebble's single-threaded batch contract.
	var afterStem func()
	if tracker != nil {
		afterStem = func() {
			_ = tracker.MaybeCalibrate(func() error {
				if sbw != nil {
					sbw.flush()
				}
				if tnw != nil {
					tnw.flush()
				}
				return nil
			})
		}
	}

	numWorkers := runtime.GOMAXPROCS(0) - 2
	if numWorkers < 2 {
		numWorkers = 2
	}
	if g.config.Verbose {
		log.Printf("Phase 2: using %d parallel workers", numWorkers)
	}
	stateRoot, tnStats, sbStats, err := computeBinaryRootStreamingParallel(
		context.Background(), iter, tnw, sbw, g.config.GroupDepth, numWorkers, afterStem,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to compute binary root: %w", err)
	}
	if g.config.Verbose {
		log.Printf("Computed binary trie root in %v", time.Since(hashStart).Round(time.Millisecond))
	}

	// Wait for code writes to complete (they ran concurrently with Phase 2).
	closeSnap()
	snapWg.Wait()
	if e := snapErr.Load(); e != nil {
		return nil, fmt.Errorf("code write failed: %w", e.(error))
	}
	if err := g.writer.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush writer: %w", err)
	}

	stats.StateRoot = stateRoot

	// Write state root via StateWriter
	if err := g.writer.SetStateRoot(stateRoot); err != nil {
		return nil, fmt.Errorf("failed to write snapshot root: %w", err)
	}

	if g.config.Verbose {
		log.Printf("State root (binary stack trie): %s", stateRoot.Hex())
		log.Printf("Generated %d accounts, %d contracts with %d total storage slots (%d trie entries)",
			stats.AccountsCreated, stats.ContractsCreated, stats.StorageSlotsCreated, totalEntries)
	}

	writerStats := g.writer.Stats()
	stats.AccountBytes = writerStats.AccountBytes
	stats.StorageBytes = writerStats.StorageBytes
	stats.CodeBytes = writerStats.CodeBytes
	stats.TrieNodeBytes = uint64(tnStats.Bytes)
	stats.StemBlobBytes = uint64(sbStats.Bytes)
	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes + stats.TrieNodeBytes + stats.StemBlobBytes

	elapsed := time.Since(start)
	stats.GenerationTime = elapsed
	stats.DBWriteTime = elapsed

	return stats, nil
}

// writeCodeOnly writes only contract code to the DB. Used in binary trie mode
// where account/storage flat state is written as stem blobs in Phase 2, but
// code still needs to be persisted under "c" + keccak256(code) for geth's
// code reader (the EVM reads full code from there, not from trie chunks).
func (g *Generator) writeCodeOnly(acc *accountData) error {
	if len(acc.code) > 0 {
		if err := g.writer.WriteCode(acc.codeHash, acc.code); err != nil {
			return fmt.Errorf("write code: %w", err)
		}
	}
	return nil
}

// storageSlot is a key-value pair for deterministic storage iteration.
type storageSlot struct {
	Key   common.Hash
	Value common.Hash
}

// accountData holds generated account data.
type accountData struct {
	address  common.Address
	addrHash common.Hash
	account  *types.StateAccount
	code     []byte
	codeHash common.Hash
	storage  []storageSlot // pre-sorted by Key for deterministic trie insertion
}

// mapToSortedSlots converts a storage map to a sorted slice of storageSlot.
func mapToSortedSlots(m map[common.Hash]common.Hash) []storageSlot {
	slots := make([]storageSlot, 0, len(m))
	for k, v := range m {
		slots = append(slots, storageSlot{Key: k, Value: v})
	}
	sort.Slice(slots, func(i, j int) bool {
		return bytes.Compare(slots[i].Key[:], slots[j].Key[:]) < 0
	})
	return slots
}

// generateEOA generates an Externally Owned Account.
func (g *Generator) generateEOA() *accountData {
	var addr common.Address
	g.rng.Read(addr[:])

	// Random balance between 0 and 1000 ETH
	balance := new(uint256.Int).Mul(
		uint256.NewInt(uint64(g.rng.Intn(1000))),
		uint256.NewInt(1e18),
	)

	return &accountData{
		address:  addr,
		addrHash: crypto.Keccak256Hash(addr[:]),
		account: &types.StateAccount{
			Nonce:    uint64(g.rng.Intn(1000)),
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
		storage: nil,
	}
}

// generateContract generates a contract account with storage.
func (g *Generator) generateContract(numSlots int) *accountData {
	var addr common.Address
	g.rng.Read(addr[:])

	// Generate random code
	codeSize := g.config.CodeSize + g.rng.Intn(g.config.CodeSize)
	code := make([]byte, codeSize)
	g.rng.Read(code)
	codeHash := crypto.Keccak256Hash(code)

	// Random balance
	balance := new(uint256.Int).Mul(
		uint256.NewInt(uint64(g.rng.Intn(100))),
		uint256.NewInt(1e18),
	)

	// Generate storage slots as a pre-sorted slice for deterministic trie insertion.
	storage := make([]storageSlot, 0, numSlots)
	for j := 0; j < numSlots; j++ {
		var key, value common.Hash
		g.rng.Read(key[:])
		g.rng.Read(value[:])
		// Ensure value is non-zero (zero values are deletions)
		if value == (common.Hash{}) {
			value[31] = 1
		}
		storage = append(storage, storageSlot{Key: key, Value: value})
	}
	sort.Slice(storage, func(i, j int) bool {
		return bytes.Compare(storage[i].Key[:], storage[j].Key[:]) < 0
	})

	return &accountData{
		address:  addr,
		addrHash: crypto.Keccak256Hash(addr[:]),
		account: &types.StateAccount{
			Nonce:    uint64(g.rng.Intn(1000)),
			Balance:  balance,
			Root:     types.EmptyRootHash, // Will be computed
			CodeHash: codeHash.Bytes(),
		},
		code:     code,
		codeHash: codeHash,
		storage:  storage,
	}
}

// generateSlotDistribution generates the number of storage slots for each contract.
func (g *Generator) generateSlotDistribution() []int {
	distribution := make([]int, g.config.NumContracts)

	switch g.config.Distribution {
	case PowerLaw:
		// Power-law distribution (Pareto) - 80/20 rule
		// Most contracts have few slots, few contracts have many
		alpha := 1.5 // Shape parameter
		for i := range distribution {
			// Inverse CDF of Pareto distribution
			u := g.rng.Float64()
			slots := float64(g.config.MinSlots) / math.Pow(1-u, 1/alpha)
			if slots > float64(g.config.MaxSlots) {
				slots = float64(g.config.MaxSlots)
			}
			distribution[i] = int(slots)
		}

	case Exponential:
		// Exponential decay
		lambda := math.Log(2) / float64(g.config.MaxSlots/4)
		for i := range distribution {
			u := g.rng.Float64()
			slots := -math.Log(1-u) / lambda
			slots = math.Max(float64(g.config.MinSlots), math.Min(slots, float64(g.config.MaxSlots)))
			distribution[i] = int(slots)
		}

	case Uniform:
		// Uniform distribution
		for i := range distribution {
			distribution[i] = g.config.MinSlots + g.rng.Intn(g.config.MaxSlots-g.config.MinSlots+1)
		}
	}

	return distribution
}

// generateSlotCount generates the slot count for a single contract using
// the configured distribution. Called from the producer goroutine (which
// owns the RNG). Each call consumes exactly 1 RNG call, so the sequence
// is identical to generateSlotDistribution for the same seed.
func (g *Generator) generateSlotCount() int {
	switch g.config.Distribution {
	case PowerLaw:
		alpha := 1.5
		u := g.rng.Float64()
		slots := float64(g.config.MinSlots) / math.Pow(1-u, 1/alpha)
		if slots > float64(g.config.MaxSlots) {
			slots = float64(g.config.MaxSlots)
		}
		return int(slots)
	case Exponential:
		lambda := math.Log(2) / float64(g.config.MaxSlots/4)
		u := g.rng.Float64()
		slots := -math.Log(1-u) / lambda
		slots = math.Max(float64(g.config.MinSlots), math.Min(slots, float64(g.config.MaxSlots)))
		return int(slots)
	case Uniform:
		return g.config.MinSlots + g.rng.Intn(g.config.MaxSlots-g.config.MinSlots+1)
	default:
		return g.config.MinSlots
	}
}

// dirSize returns the total size of all files in a directory tree.
func dirSize(path string) (uint64, error) {
	var total uint64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

// encodeStorageValue encodes a storage value using RLP with leading zeros trimmed.
func encodeStorageValue(value common.Hash) ([]byte, error) {
	trimmed := trimLeftZeroes(value[:])
	if len(trimmed) == 0 {
		return nil, nil
	}
	encoded, err := rlp.EncodeToBytes(trimmed)
	if err != nil {
		return nil, fmt.Errorf("failed to RLP-encode storage value %x: %w", value, err)
	}
	return encoded, nil
}

func trimLeftZeroes(s []byte) []byte {
	for i, v := range s {
		if v != 0 {
			return s[i:]
		}
	}
	return nil
}


func formatBytesInternal(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
