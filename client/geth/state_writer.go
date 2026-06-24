package geth

import (
	"bytes"
	"context"
	"fmt"
	"log"
	mrand "math/rand"
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

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/streamingtrie"
	"github.com/ethereum/state-actor/internal/streamsort"
)

const phase1FlushBytes = 64 * 1024 * 1024

// parallelKeccakThreshold is the slot count above which a contract's
// keccak hashing is parallelised across cores.
const parallelKeccakThreshold = 64

// maxPhase0Workers caps Phase 0 drain-and-compute parallelism so peak
// per-worker streamsort RAM stays within the design budget.
const maxPhase0Workers = 8

// scratchBatchFlushBytes is the per-worker batch flush threshold during
// Phase 0. Matches defaultFlushBytes in writer.go (64 MiB).
const scratchBatchFlushBytes = 64 * 1024 * 1024

// writeStateAndCollectRoot drives the two-phase MPT pipeline.
//
// Phase 0 streams each spec-PreAlloc entity's storage iter through
// streamingtrie, persisting per-slot snapshot rows + storage-trie nodes
// and splicing the computed root into cfg.GenesisAccounts[addr].Root.
//
// Phase 1 streams entitygen output (genesis-alloc, synthetic EOAs +
// contracts) into a streamsort.Store keyed by addrHash for Phase 2 to
// consume sorted. Phase 2 builds per-account storage tries, writes the
// production Pebble in keccak order, and feeds the outer account trie.
//
// Memory is bounded by O(max storage slots in any single contract).
func writeStateAndCollectRoot(
	ctx context.Context,
	cfg generator.Config,
	w *Writer,
) (common.Hash, *generator.Stats, error) {
	stats := &generator.Stats{}
	start := time.Now()

	sorter, err := streamsort.New("")
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("geth: streamsort.New: %w", err)
	}
	defer sorter.Close()

	// hashToAddr lets Phase 2 look up cfg.GenesisAccounts[addr].Root
	// (set by Phase 0) when encoding spec-entity account leaves.
	hashToAddr := make(map[common.Hash]common.Address, len(cfg.GenesisAccounts))
	for addr := range cfg.GenesisAccounts {
		hashToAddr[crypto.Keccak256Hash(addr[:])] = addr
	}

	if err := runPhase0(ctx, cfg, w); err != nil {
		return common.Hash{}, nil, err
	}

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))

	// genesisAddrs prevents synthetic RNG addresses from colliding with
	// pre-allocated genesis addresses.
	genesisAddrs := make(map[common.Address]struct{}, len(cfg.GenesisAccounts))

	cfg.Progress.Stage("geth: phase 1/2 — generating accounts")

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
		if err := sorter.Put(addrHash[:], blob); err != nil {
			return common.Hash{}, nil, fmt.Errorf("phase1 genesis alloc: %w", err)
		}
	}

	plan := cfg.AutoFill
	if plan != nil {
		for i := 0; i < plan.NumEOAs; i++ {
			if err := ctx.Err(); err != nil {
				return common.Hash{}, nil, err
			}
			acc := plan.DrawEOA(rng)
			for _, dup := genesisAddrs[acc.Address]; dup; {
				acc = plan.DrawEOA(rng)
				_, dup = genesisAddrs[acc.Address]
			}
			var blob []byte
			if len(acc.Code) > 0 {
				blob = encodeEntityContract(acc.StateAccount.Nonce, acc.StateAccount.Balance, acc.Code, nil)
			} else {
				blob = encodeEntityEOA(acc.StateAccount.Nonce, acc.StateAccount.Balance)
			}
			if err := sorter.Put(acc.AddrHash[:], blob); err != nil {
				return common.Hash{}, nil, fmt.Errorf("phase1 EOA #%d: %w", i, err)
			}
			stats.AccountsCreated++
			if len(stats.SampleEOAs) < 3 {
				stats.SampleEOAs = append(stats.SampleEOAs, acc.Address)
			}
			cfg.Progress.Tick(int64(i+1), int64(plan.NumEOAs), "EOAs")
		}

		for i := 0; i < plan.NumContracts; i++ {
			if err := ctx.Err(); err != nil {
				return common.Hash{}, nil, err
			}
			contract := plan.DrawContract(rng)
			for _, dup := genesisAddrs[contract.Address]; dup; {
				contract = plan.DrawContract(rng)
				_, dup = genesisAddrs[contract.Address]
			}

			// contract.Storage is sorted by raw Key; Phase 2 re-sorts by keccak(Key).
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
			if err := sorter.Put(contract.AddrHash[:], blob); err != nil {
				return common.Hash{}, nil, fmt.Errorf("phase1 contract #%d: %w", i, err)
			}
			stats.ContractsCreated++
			stats.StorageSlotsCreated += len(contract.Storage)
			if len(stats.SampleContracts) < 3 {
				stats.SampleContracts = append(stats.SampleContracts, contract.Address)
			}
			cfg.Progress.Tick(int64(i+1), int64(plan.NumContracts),
				fmt.Sprintf("contracts · %d slots", stats.StorageSlotsCreated))
		}
	}

	stats.GenerationTime = time.Since(start)
	if cfg.Verbose {
		log.Printf("[geth MPT Phase 1] complete: %d accounts, %d contracts, %d slots in %v",
			stats.AccountsCreated, stats.ContractsCreated,
			stats.StorageSlotsCreated, stats.GenerationTime.Round(time.Millisecond))
	}

	phase2Start := time.Now()

	cfg.Progress.Stage("geth: phase 2/2 — building account trie")
	phase2Total := int64(stats.AccountsCreated + stats.ContractsCreated)

	// Outer account trie nodes are persisted under TrieNodeAccountPrefix.
	// Geth's PathDB boot derives the trie root via
	// keccak256(rawdb.ReadAccountTrieNode(db, nil)); a missing root node
	// short-circuits to EmptyRootHash and fails snapshot consistency.
	var accountTrieErr error
	accountCb := func(path []byte, hash common.Hash, blob []byte) {
		if accountTrieErr != nil {
			return
		}
		// StackTrie's path/blob slices are volatile — copy before queuing.
		p := append([]byte(nil), path...)
		b := append([]byte(nil), blob...)
		key := append([]byte{}, rawdb.TrieNodeAccountPrefix...)
		key = append(key, p...)
		if err := w.PutTrieNode(key, b); err != nil {
			accountTrieErr = fmt.Errorf("geth: write account trie node: %w", err)
		}
	}
	accountTrie := trie.NewStackTrie(accountCb)

	var codeSeenCap int
	if cfg.AutoFill != nil {
		codeSeenCap = cfg.AutoFill.NumContracts
	}
	codeSeen := make(map[common.Hash]struct{}, codeSeenCap)

	count := 0
	iterErr := sorter.Iterate(func(key, value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var addrHash common.Hash
		copy(addrHash[:], key)

		ent, err := decodeEntityBlob(value)
		if err != nil {
			return fmt.Errorf("phase2 decode at #%d: %w", count, err)
		}

		// Spec entities stream their storage in Phase 0 (ent.slots is
		// empty for them); pick up the pre-computed Root from
		// cfg.GenesisAccounts instead of recomputing.
		storageRoot, sortedSlotEntries, err := buildStorageTrie(w, addrHash, ent.slots)
		if err != nil {
			return fmt.Errorf("phase2 storage trie at #%d: %w", count, err)
		}
		if len(ent.slots) == 0 {
			if specAddr, ok := hashToAddr[addrHash]; ok {
				if acc := cfg.GenesisAccounts[specAddr]; acc != nil &&
					acc.Root != (common.Hash{}) {
					storageRoot = acc.Root
				}
			}
		}

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
			return fmt.Errorf("phase2 write account at #%d: %w", count, err)
		}
		for _, s := range sortedSlotEntries {
			if err := w.WriteStorageRLP(addrHash, s.slotHash, s.valueRLP); err != nil {
				return fmt.Errorf("phase2 write slot at #%d: %w", count, err)
			}
		}
		if len(ent.code) > 0 {
			if _, dup := codeSeen[codeHash]; !dup {
				if err := w.WriteCode(codeHash, ent.code); err != nil {
					return fmt.Errorf("phase2 write code at #%d: %w", count, err)
				}
				codeSeen[codeHash] = struct{}{}
			}
		}

		// MUST be full StateAccount RLP (not SlimAccountRLP) — geth's
		// trie reader expects a fixed 32-byte Root field.
		fullRLP, err := rlp.EncodeToBytes(&acc)
		if err != nil {
			return fmt.Errorf("phase2 encode account RLP at #%d: %w", count, err)
		}
		if err := accountTrie.Update(addrHash[:], fullRLP); err != nil {
			return fmt.Errorf("phase2 account trie update at #%d: %w", count, err)
		}

		count++
		cfg.Progress.Tick(int64(count), phase2Total, "accounts")
		return nil
	})
	if iterErr != nil {
		return common.Hash{}, nil, iterErr
	}
	if accountTrieErr != nil {
		return common.Hash{}, nil, accountTrieErr
	}

	stateRoot := accountTrie.Hash()
	// accountTrie.Hash() may emit final nodes through the callback.
	if accountTrieErr != nil {
		return common.Hash{}, nil, accountTrieErr
	}
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

	return stateRoot, stats, nil
}

// sortedSlot is a (keccak(slotKey), RLP value) pair sorted by slotHash.
type sortedSlot struct {
	slotHash common.Hash
	valueRLP []byte
}

// buildStorageTrie hashes + sorts the slots, builds the per-account
// storage StackTrie (always emitting trie nodes via w.PutTrieNode —
// geth's PathDB requires them at boot), and returns (root, sortedEntries)
// for the caller to write the snapshot in keccak order. For ≥
// parallelKeccakThreshold slots, keccak hashing is parallelised.
func buildStorageTrie(
	w *Writer,
	accountHash common.Hash,
	slots []entityBlobSlot,
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

	acctHash := accountHash // capture for closure
	var storageTrieErr error
	storageCb := func(path []byte, hash common.Hash, blob []byte) {
		if storageTrieErr != nil {
			return
		}
		p := append([]byte(nil), path...)
		b := append([]byte(nil), blob...)
		key := make([]byte, 0, len(rawdb.TrieNodeStoragePrefix)+common.HashLength+len(p))
		key = append(key, rawdb.TrieNodeStoragePrefix...)
		key = append(key, acctHash[:]...)
		key = append(key, p...)
		if err := w.PutTrieNode(key, b); err != nil {
			storageTrieErr = fmt.Errorf("geth: write storage trie node: %w", err)
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
	root := storageTrie.Hash()
	if storageTrieErr != nil {
		return common.Hash{}, nil, storageTrieErr
	}
	return root, out, nil
}

// gethStorageHashBuilder adapts trie.StackTrie to
// streamingtrie.HashBuilder. The StackTrie callback persists each
// storage-trie node under TrieNodeStoragePrefix + addrHash + path
// (required by geth's PathDB). PutTrieNode failures are captured in
// err (sticky) and surfaced via AddLeaf / Root.
type gethStorageHashBuilder struct {
	t        *trie.StackTrie
	w        *Writer
	addrHash common.Hash
	err      error
}

func newGethStorageHashBuilder(w *Writer, addrHash common.Hash) *gethStorageHashBuilder {
	hb := &gethStorageHashBuilder{w: w, addrHash: addrHash}
	cb := func(path []byte, hash common.Hash, blob []byte) {
		if hb.err != nil {
			return
		}
		p := append([]byte(nil), path...)
		b := append([]byte(nil), blob...)
		key := make([]byte, 0, len(rawdb.TrieNodeStoragePrefix)+common.HashLength+len(p))
		key = append(key, rawdb.TrieNodeStoragePrefix...)
		key = append(key, hb.addrHash[:]...)
		key = append(key, p...)
		if err := hb.w.PutTrieNode(key, b); err != nil {
			hb.err = fmt.Errorf("geth: write storage trie node for %s: %w", hb.addrHash.Hex(), err)
		}
	}
	hb.t = trie.NewStackTrie(cb)
	return hb
}

// scratchBatchWriter owns a *pebble.Batch and auto-flushes when it crosses
// `threshold` bytes. Owned by a single worker goroutine; not thread-safe.
//
// Why this exists: a single bloated EOA writes >30 GiB of slot snapshot
// rows + storage-trie nodes during one streamingtrie.IterateRoot call.
// Pebble's *Batch has a 4 GiB hard size limit (cockroachdb/pebble/batch.go
// :maxBatchSize) and will panic if exceeded. A check after IterateRoot
// returns is too late; flushing must happen mid-iteration. This wrapper
// makes every Set check the size and rotate the batch when full.
type scratchBatchWriter struct {
	w         *Writer
	batch     *pebble.Batch
	threshold int
}

func newScratchBatchWriter(w *Writer, threshold int) *scratchBatchWriter {
	return &scratchBatchWriter{w: w, batch: w.NewScratchBatch(), threshold: threshold}
}

// Set forwards to the current batch and rotates+commits when crossing
// the size threshold. The returned error short-circuits any caller.
func (sbw *scratchBatchWriter) Set(key, value []byte) error {
	if err := sbw.batch.Set(key, value, nil); err != nil {
		return err
	}
	if sbw.batch.Len() >= sbw.threshold {
		if err := sbw.w.CommitScratchBatch(sbw.batch); err != nil {
			return err
		}
		sbw.batch = sbw.w.NewScratchBatch()
	}
	return nil
}

// Flush commits any pending bytes. Idempotent; safe to call when empty.
func (sbw *scratchBatchWriter) Flush() error {
	if sbw.batch.Len() > 0 {
		if err := sbw.w.CommitScratchBatch(sbw.batch); err != nil {
			return err
		}
		sbw.batch = sbw.w.NewScratchBatch()
	}
	return nil
}

// Close commits any pending bytes and releases the current batch.
func (sbw *scratchBatchWriter) Close() error {
	if sbw.batch.Len() > 0 {
		return sbw.w.CommitScratchBatch(sbw.batch)
	}
	return sbw.batch.Close()
}

// newScratchGethStorageHashBuilder builds a HashBuilder that writes
// storage-trie nodes through a *scratchBatchWriter (which rotates the
// underlying *pebble.Batch when full). Used by Phase 0's worker pool
// so each worker writes without contending on the shared w.batch+batchMu
// AND without panicking when a bloated entity writes >4 GiB of trie
// nodes in one IterateRoot call.
//
// The returned builder reuses gethStorageHashBuilder; only the callback
// differs. err remains per-instance (per-worker).
func newScratchGethStorageHashBuilder(sbw *scratchBatchWriter, addrHash common.Hash) *gethStorageHashBuilder {
	hb := &gethStorageHashBuilder{addrHash: addrHash}
	cb := func(path []byte, hash common.Hash, blob []byte) {
		if hb.err != nil {
			return
		}
		p := append([]byte(nil), path...)
		b := append([]byte(nil), blob...)
		key := make([]byte, 0, len(rawdb.TrieNodeStoragePrefix)+common.HashLength+len(p))
		key = append(key, rawdb.TrieNodeStoragePrefix...)
		key = append(key, hb.addrHash[:]...)
		key = append(key, p...)
		if err := sbw.Set(key, b); err != nil {
			hb.err = fmt.Errorf("geth: scratch trie node Set for %s: %w", hb.addrHash.Hex(), err)
		}
	}
	hb.t = trie.NewStackTrie(cb)
	return hb
}

func (g *gethStorageHashBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	if g.err != nil {
		return g.err
	}
	return g.t.Update(keyHash[:], valueRLP)
}

func (g *gethStorageHashBuilder) Root() (common.Hash, error) {
	root := g.t.Hash() // triggers final node-completion callbacks
	if g.err != nil {
		return common.Hash{}, g.err
	}
	return root, nil
}

// runPhase0 drives the spec-PreAlloc streaming-trie phase in parallel.
//
// For every cfg.PreAlloc entity with pe.Storage != nil, a worker:
//  1. Drains the entity's storage iter into a per-call streamsort.Store
//     (rooted under cfg.DBPath so the temp lives on the production
//     filesystem, not the docker container's /tmp overlay).
//  2. Iterates the sorted store, building a per-account storage MPT
//     via a scratch-batch HashBuilder. Per-slot snapshot rows and
//     per-storage-trie-node writes go to the worker's own *pebble.Batch
//     — no contention on the shared w.batch+batchMu hot path.
//  3. Flushes its batch via w.CommitScratchBatch when it crosses
//     scratchBatchFlushBytes, and one final flush at worker exit.
//  4. Reports the computed storage root through preparedCh to the main
//     goroutine, which assigns it to cfg.GenesisAccounts[addr].Root.
//
// Root assignment is single-writer on the main goroutine; workers
// never touch cfg.GenesisAccounts. Across-entity Pebble keyspaces are
// disjoint (every key is addrHash-prefixed), so parallel db.Apply
// calls don't conflict at the storage layer; they coalesce in
// Pebble's WAL pipeline.
func runPhase0(ctx context.Context, cfg generator.Config, w *Writer) error {
	indices := make([]int, 0, len(cfg.PreAlloc))
	for i := range cfg.PreAlloc {
		if cfg.PreAlloc[i].Storage != nil {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return nil
	}

	// Phase 0 streams each spec entity's storage slots (100M–1B per bloat
	// entity) and is otherwise silent for hours. The count-only slot heartbeat
	// funnels every worker's per-slot count through one SlotMeter; each worker
	// holds its own SlotWorker so the hot per-slot path stays cheap (one
	// non-atomic add + mask).
	cfg.Progress.Stage("geth: phase 0 — spec storage")
	slotMeter := cfg.Progress.SlotMeter()

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > maxPhase0Workers {
		workers = maxPhase0Workers
	}
	if workers > len(indices) {
		workers = len(indices)
	}

	type preparedEntity struct {
		idx  int
		addr common.Address
		root common.Hash
	}

	drainCtx, cancelDrain := context.WithCancelCause(ctx)
	defer cancelDrain(nil)

	drainCh := make(chan int, workers*2)
	preparedCh := make(chan preparedEntity, workers*4)

	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slotW := slotMeter.Worker()
			sbw := newScratchBatchWriter(w, scratchBatchFlushBytes)
			defer func() {
				if err := sbw.Close(); err != nil {
					cancelDrain(fmt.Errorf("geth phase0: final flush: %w", err))
				}
			}()

			for i := range drainCh {
				if drainCtx.Err() != nil {
					return
				}
				pe := &cfg.PreAlloc[i]
				addr := pe.Address
				addrHash := crypto.Keccak256Hash(addr[:])

				d, err := streamingtrie.Drain(cfg.DBPath, pe.Storage)
				if err != nil {
					cancelDrain(fmt.Errorf("geth: drain spec storage[%d] %s: %w", i, addr.Hex(), err))
					return
				}
				sink := func(keyHash, _rawKey, value common.Hash) error {
					slotW.Slot()
					valueRLP, encErr := encodeStorageValue(value)
					if encErr != nil {
						return encErr
					}
					key := storageSnapshotKey(addrHash, keyHash)
					return sbw.Set(key, valueRLP)
				}
				hb := newScratchGethStorageHashBuilder(sbw, addrHash)
				root, err := d.IterateRoot(hb, sink)
				d.Close()
				if err != nil {
					cancelDrain(fmt.Errorf("geth: iterate spec storage[%d] %s: %w", i, addr.Hex(), err))
					return
				}
				select {
				case preparedCh <- preparedEntity{idx: i, addr: addr, root: root}:
				case <-drainCtx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(drainCh)
		for _, i := range indices {
			select {
			case drainCh <- i:
			case <-drainCtx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(preparedCh)
	}()

	for entry := range preparedCh {
		if acc, ok := cfg.GenesisAccounts[entry.addr]; ok && acc != nil {
			acc.Root = entry.root
		}
	}
	if cause := context.Cause(drainCtx); cause != nil && cause != context.Canceled {
		return cause
	}
	return nil
}
