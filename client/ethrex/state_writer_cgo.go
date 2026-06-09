//go:build cgo_ethrex

package ethrex

import (
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	ethrexinternal "github.com/nerolation/state-actor/internal/ethrex"
	"github.com/nerolation/state-actor/internal/streamingtrie"
	"github.com/nerolation/state-actor/internal/streamsort"
)

// writeState builds the full ethrex state from cfg and writes it to db.
// Returns the state root hash and stats.
//
// Pipeline:
//  1. Handle PreAlloc storage: for each PreAlloc entity with storage,
//     build the storage trie in-memory and record its root.
//  2. Queue all entities (AutoFill EOAs, AutoFill contracts, GenesisAccounts)
//     into a streamsort.Store keyed by addrHash.
//  3. Iterate sorted with a 3-stage parallel pipeline:
//     Stage A (reader): pull (addrHash, entity) in addrHash order, stamp seq.
//     Stage B (N workers): compute storage trie in memory for normal accounts.
//     Stage C (writer): apply results in strict seq order — always addrHash-sorted.
//
// RAM bound: internal/ethrex.Builder accumulates all leaves in memory. This
// is correct for the e2e fixture and moderate --target-size. See doc.go for
// the ceiling note.
//
// Flat-KV / snap-sync layout: ethrex serves state reads from the flat-KV CFs
// once a background task has swept the trie post-sync. We pre-populate them so
// the produced DB models a SYNCED node (matching how every other client fakes a
// synced flat layer). Specifically we model a snap-synced node: each leaf
// full-path row (the only rows whose key ends in the leaf-flag nibble) is routed
// ONLY to the flat-KV CF, never duplicated into the trie-node CF — exactly how
// ethrex's own apply_trie_updates and snap-sync bulk builder persist state. The
// trie-node CFs keep the structural and leaf-NODE-RLP rows (which carry the value
// for root/proofs), so the trie is complete. writeState then stamps
// misc_values["last_written"]=0xff so ethrex skips regeneration on boot.
func writeState(
	ctx context.Context,
	cfg generator.Config,
	db *ethrexDB,
	accountSink *batchSink,
	storageSink *batchSink,
	accountFkvSink *batchSink,
	storageFkvSink *batchSink,
	codeSink *batchSink,
	codeMetaSink *batchSink,
) (common.Hash, *generator.Stats, error) {
	stats := &generator.Stats{}

	emptyCodeHash := common.HexToHash(ethrexinternal.EmptyCodeHashHex)
	emptyTrieHash := common.HexToHash(ethrexinternal.EmptyTrieHashHex)

	// seenCodeHash deduplicates account_codes + account_code_metadata writes.
	// Owned exclusively by Stage C.
	seenCodeHash := make(map[common.Hash]struct{})

	// writeCode writes code for a given codeHash if not already seen.
	// Always writes even for empty code (ethrex stores the empty-code entry).
	writeCode := func(codeHash common.Hash, code []byte) error {
		if _, seen := seenCodeHash[codeHash]; seen {
			return nil
		}
		seenCodeHash[codeHash] = struct{}{}
		encoded := ethrexinternal.EncodeCode(code)
		if err := codeSink.put(codeHash[:], encoded); err != nil {
			return fmt.Errorf("ethrex: put account_codes: %w", err)
		}
		meta := ethrexinternal.CodeLengthMetadata(code)
		if err := codeMetaSink.put(codeHash[:], meta[:]); err != nil {
			return fmt.Errorf("ethrex: put account_code_metadata: %w", err)
		}
		return nil
	}

	// Write empty-code entry up-front (every account needs it written once).
	if err := writeCode(emptyCodeHash, nil); err != nil {
		return common.Hash{}, nil, err
	}

	// isLeafFullPath reports whether a node row is a leaf's full-path row (the
	// "row 2" the Builder emits as path++rem++[LeafFlag]). These are the only
	// rows whose key ends in the leaf-flag nibble; branch/extension/leaf-node-RLP
	// rows and the empty-trie sentinel never do.
	isLeafFullPath := func(path []byte) bool {
		return len(path) > 0 && path[len(path)-1] == ethrexinternal.LeafFlag
	}

	// accountTrieNodeSink routes the account trie's emitted rows in the snap-sync
	// layout: a leaf full-path row goes ONLY to account_flatkeyvalue, never to
	// account_trie_nodes. This mirrors how ethrex itself persists state — its
	// apply_trie_updates routes leaf rows (len 65/131) to the flat-KV CF and only
	// structural/leaf-node rows to the trie-node CF, and its snap-sync bulk
	// builder (trie_from_sorted) writes no leaf full-path rows either. The leaf
	// NODE RLP (row 1) still lands in account_trie_nodes and carries the value for
	// root/proof computation, so the on-disk trie is complete without the
	// duplicate row-2 entry. A genesis-booted node would carry that duplicate; a
	// snap-synced node does not — modelling the latter keeps the DB representative
	// and smaller.
	accountTrieNodeSink := ethrexinternal.NodeSink(func(path []byte, value []byte) error {
		if isLeafFullPath(path) {
			return accountFkvSink.put(path, value)
		}
		return accountSink.put(path, value)
	})

	// storageTrieNodeSink routes the storage tries' emitted rows. Paths arrive
	// already address-prefixed (PrefixedSink), so a leaf full-path row is
	// byte-identical to ethrex's apply_prefix(account, slot) flat-KV key. Same
	// snap-sync split: leaf full-path → storage_flatkeyvalue only; structural and
	// leaf-node rows → storage_trie_nodes.
	storageTrieNodeSink := ethrexinternal.NodeSink(func(path []byte, value []byte) error {
		if isLeafFullPath(path) {
			return storageFkvSink.put(path, value)
		}
		return storageSink.put(path, value)
	})

	// preAllocStorageRoots maps addrHash → computed storage root for PreAlloc
	// entities whose storage was built in-memory.
	preAllocStorageRoots := make(map[common.Hash]common.Hash)

	// Phase 0: handle PreAlloc entities with storage. Each entity's Storage is
	// a re-iterable streaming iterator (iter.Seq2); draining it through
	// internal/streamingtrie sorts on disk (streamsort) and replays slots in
	// keccak-ascending order into the streaming Builder. Peak RAM is bounded by
	// the streamsort memtable + the O(keyLen) Builder, so a single huge-storage
	// contract (100M-1B slots) never materializes. Mirrors reth's
	// spec_storage_streaming_cgo.go.
	for i := range cfg.PreAlloc {
		pe := &cfg.PreAlloc[i]
		if pe.Storage == nil {
			continue
		}
		if ctx.Err() != nil {
			return common.Hash{}, nil, ctx.Err()
		}
		addr := pe.Address
		addrHash := crypto.Keccak256Hash(addr[:])

		// Storage rows go through the 66-nibble storage prefix. Suppress the
		// empty-trie sentinel row ([] -> 0x80): streamingtrie always calls
		// Builder.Root(), which for storage that drains to zero non-zero slots
		// would otherwise write a bogus (prefix, 0x80) row. The pre-streaming
		// code wrote zero rows for empty storage; this preserves that exactly
		// (and the returned root is still emptyTrieHash, as before).
		prefixedSink := ethrexinternal.PrefixedSink(addrHash, storageTrieNodeSink)
		hb := ethrexinternal.NewStreamHashBuilder(ethrexinternal.SuppressEmptyTrieSentinel(prefixedSink))

		// Stats-only sink: storage rows are emitted by the Builder via
		// prefixedSink; this only counts slots/bytes. The encoded length here
		// equals streamingtrie's internal value RLP length, proven byte-identical
		// by internal/ethrex TestStorageValueEncodingMatchesStreamingtrie.
		statsSink := func(_, _, value common.Hash) error {
			enc := ethrexinternal.EncodeStorageValue(new(uint256.Int).SetBytes32(value[:]))
			stats.StorageSlotsCreated++
			stats.StorageBytes += uint64(len(enc))
			return nil
		}

		root, err := streamingtrie.StorageRoot(cfg.DBPath, pe.Storage, hb, statsSink)
		if err != nil {
			return common.Hash{}, nil, fmt.Errorf("ethrex: storage root (PreAlloc %s): %w", addr.Hex(), err)
		}
		preAllocStorageRoots[addrHash] = root

		// Splice root into the GenesisAccounts entry (same pointer as Phase 1 needs).
		if acc, ok := cfg.GenesisAccounts[addr]; ok && acc != nil {
			acc.Root = root
		}
	}

	// Phase 1: queue all entities into a streamsort keyed by addrHash.
	sorter, err := streamsort.New("")
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: streamsort.New: %w", err)
	}
	defer sorter.Close()

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))

	plan := cfg.AutoFill
	if plan != nil {
		for i := 0; i < plan.NumEOAs; i++ {
			if ctx.Err() != nil {
				return common.Hash{}, nil, ctx.Err()
			}
			acc := plan.DrawEOA(rng)
			blob := encodeEntity(acc.StateAccount.Nonce, acc.StateAccount.Balance, acc.Code, nil)
			if err := sorter.Put(acc.AddrHash[:], blob); err != nil {
				return common.Hash{}, nil, err
			}
		}
	}

	seenAlloc := make(map[common.Address]struct{}, len(cfg.GenesisAccounts))
	for addr, acc := range cfg.GenesisAccounts {
		if ctx.Err() != nil {
			return common.Hash{}, nil, ctx.Err()
		}
		if _, dup := seenAlloc[addr]; dup {
			continue
		}
		seenAlloc[addr] = struct{}{}
		addrHash := crypto.Keccak256Hash(addr[:])
		balance := acc.Balance
		if balance == nil {
			balance = uint256.NewInt(0)
		}
		code := cfg.GenesisCode[addr]
		storage := cfg.GenesisStorage[addr]
		blob := encodeEntity(acc.Nonce, balance, code, storage)
		if err := sorter.Put(addrHash[:], blob); err != nil {
			return common.Hash{}, nil, err
		}
	}

	if plan != nil {
		for i := 0; i < plan.NumContracts; i++ {
			if ctx.Err() != nil {
				return common.Hash{}, nil, ctx.Err()
			}
			contract := plan.DrawContract(rng)
			slotMap := make(map[common.Hash]common.Hash, len(contract.Storage))
			for _, s := range contract.Storage {
				slotMap[s.Key] = s.Value
			}
			blob := encodeEntity(contract.StateAccount.Nonce, contract.StateAccount.Balance, contract.Code, slotMap)
			if err := sorter.Put(contract.AddrHash[:], blob); err != nil {
				return common.Hash{}, nil, err
			}
		}
	}

	// Phase 2: 3-stage ordered parallel pipeline.
	//
	// Ordering guarantee: all four CF write streams (account_trie_nodes,
	// account_flatkeyvalue, storage_trie_nodes, storage_flatkeyvalue) stay
	// addrHash-ascending because Stage C applies every account in seq (= sorter
	// output) order. Workers compute storage tries in memory without writing
	// RocksDB; only Stage C writes — in strict seq order via the reorder heap.
	//
	// Stage A (single reader goroutine):
	//   sorter.Iterate yields (addrHash, entity) in addrHash-ascending order.
	//   Each item is stamped with a monotonically increasing seq and sent to
	//   the work channel. Accounts with >parallelStorageSlotThreshold slots are
	//   flagged bigAccount so Stage C handles their storage inline.
	//
	// Stage B (numWorkers goroutines):
	//   Each worker computes the storage trie entirely in memory (no RocksDB
	//   access), producing an accountResult with buffered rows and storageRoot.
	//   Workers do not share mutable state.
	//
	// Stage C (single writer goroutine):
	//   A min-heap reorder buffer drains results in strict seq order.
	//   Per account: (1) replay buffered storage rows into storageSink/storageFkvSink
	//   (or build inline for big accounts); (2) dedup and write code; (3) add
	//   account leaf to accountBuilder; (4) accumulate stats.

	accountBuilder := ethrexinternal.NewBuilder(accountTrieNodeSink)

	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}

	// Bounded channels: cap in-flight work at 2*numWorkers so buffered storage
	// row memory is bounded and Stage A cannot race far ahead of Stage C.
	workCh := make(chan *accountWorkItem, 2*numWorkers)
	resultCh := make(chan *accountResult, 2*numWorkers)

	// pipelineCtx is cancelled on any error to abort all goroutines.
	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()

	// firstErr captures the first error from any goroutine (reader or writer).
	var firstErr error
	var errMu sync.Mutex
	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancelPipeline()
		}
		errMu.Unlock()
	}

	// Stage A: single reader goroutine — stamps seq and dispatches work items.
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		defer close(workCh)
		var seq uint64
		iterErr := sorter.Iterate(func(key, value []byte) error {
			if pipelineCtx.Err() != nil {
				return pipelineCtx.Err()
			}
			var addrHash common.Hash
			copy(addrHash[:], key)
			ent := decodeEntity(value)

			item := &accountWorkItem{
				seq:      seq,
				addrHash: addrHash,
				ent:      ent,
			}
			seq++

			if preRoot, ok := preAllocStorageRoots[addrHash]; ok {
				item.hasPreAllocRoot = true
				item.preAllocRoot = preRoot
			}

			// Big accounts are flagged so Stage C processes them inline.
			if !item.hasPreAllocRoot && len(ent.slots) > parallelStorageSlotThreshold {
				item.bigAccount = true
			}

			select {
			case workCh <- item:
				return nil
			case <-pipelineCtx.Done():
				return pipelineCtx.Err()
			}
		})
		if iterErr != nil {
			setErr(iterErr)
		}
	}()

	// Stage B: worker pool — pure in-memory storage trie computation.
	var workerWg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for item := range workCh {
				if pipelineCtx.Err() != nil {
					// Pipeline cancelled; drain workCh to unblock Stage A.
					continue
				}
				res := computeAccountResult(item, emptyCodeHash, emptyTrieHash)
				select {
				case resultCh <- res:
				case <-pipelineCtx.Done():
					continue
				}
			}
		}()
	}

	// Close resultCh once all workers finish.
	go func() {
		workerWg.Wait()
		close(resultCh)
	}()

	// Stage C: single writer goroutine — strictly in-seq application.
	// Runs on the calling goroutine after launching Stage A and B goroutines.
	reorder := newResultHeap()
	var nextSeq uint64
	var writerErr error

	applyResult := func(res *accountResult) error {
		if res.buildErr != nil {
			return res.buildErr
		}

		storageRoot := res.storageRoot

		if res.bigAccount {
			// Build storage inline (directly into RocksDB) at this seq position.
			var slotCount, slotBytes uint64
			var inlineErr error
			storageRoot, slotCount, slotBytes, inlineErr = buildStorageTrieInline(
				res.addrHash, res.bigEnt, emptyTrieHash, storageSink, storageFkvSink,
			)
			if inlineErr != nil {
				return inlineErr
			}
			res.stats.StorageSlotsCreated = slotCount
			res.stats.StorageBytes = slotBytes
		} else {
			// Replay buffered storage rows into RocksDB in their captured order.
			for _, row := range res.storageRows {
				if isLeafFullPath(row.path) {
					if err := storageFkvSink.put(row.path, row.value); err != nil {
						return err
					}
				} else {
					if err := storageSink.put(row.path, row.value); err != nil {
						return err
					}
				}
			}
		}

		// Write code (dedup via seenCodeHash; Stage C owns this map).
		if len(res.code) > 0 {
			if err := writeCode(res.codeHash, res.code); err != nil {
				return err
			}
		}

		// Recompute accountRLP for big accounts now that storageRoot is known.
		accountRLP := res.accountRLP
		if res.bigAccount {
			accountRLP = ethrexinternal.EncodeAccountState(
				res.bigEnt.nonce, res.bigEnt.balance, storageRoot, res.codeHash,
			)
			res.stats.AccountBytes = uint64(len(accountRLP))
			res.stats.IsContract = len(res.code) != 0 || storageRoot != emptyTrieHash
		}

		if err := accountBuilder.AddLeaf(ethrexinternal.BytesToNibbles(res.addrHash[:]), accountRLP); err != nil {
			return fmt.Errorf("ethrex: account leaf: %w", err)
		}

		stats.StorageSlotsCreated += int(res.stats.StorageSlotsCreated)
		stats.StorageBytes += res.stats.StorageBytes
		stats.AccountBytes += res.stats.AccountBytes
		stats.CodeBytes += res.stats.CodeBytes
		if res.stats.IsContract {
			stats.ContractsCreated++
		} else {
			stats.AccountsCreated++
		}
		return nil
	}

	for res := range resultCh {
		if writerErr != nil {
			// Pipeline error already set; drain resultCh to let goroutines finish.
			continue
		}
		heap.Push(reorder, res)
		// Drain all consecutively in-seq results.
		for reorder.Len() > 0 && (*reorder)[0].seq == nextSeq {
			next := heap.Pop(reorder).(*accountResult)
			if err := applyResult(next); err != nil {
				writerErr = err
				setErr(err)
				break
			}
			nextSeq++
		}
	}

	// Wait for reader to complete (it may have set firstErr).
	readerWg.Wait()

	errMu.Lock()
	pipelineErr := firstErr
	errMu.Unlock()
	if pipelineErr != nil {
		return common.Hash{}, nil, pipelineErr
	}
	if writerErr != nil {
		return common.Hash{}, nil, writerErr
	}

	// Finalize account trie.
	stateRoot, err := accountBuilder.Root()
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: account trie root: %w", err)
	}

	// Flush trie + flat-KV sinks.
	if err := accountSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := storageSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := accountFkvSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := storageFkvSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}

	// Mark the flat-KV layer fully generated so ethrex's background generator
	// short-circuits on boot instead of clearing + rebuilding the CFs we wrote.
	if err := db.putSync(cfIdxMiscValues, ethrexinternal.MiscValuesLastWrittenKey, ethrexinternal.FKVLastWrittenComplete); err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: put misc_values last_written: %w", err)
	}

	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return stateRoot, stats, nil
}

// computeAccountResult is the pure per-account computation run by Stage B workers.
// It does not touch RocksDB or any shared state.
//
// For bigAccount items: skips storage building, populates bigEnt so Stage C can
// build inline. Still computes codeHash (pure, no I/O).
// For hasPreAllocRoot items: uses the pre-computed root directly.
func computeAccountResult(item *accountWorkItem, emptyCodeHash, emptyTrieHash common.Hash) *accountResult {
	res := &accountResult{
		seq:        item.seq,
		addrHash:   item.addrHash,
		bigAccount: item.bigAccount,
	}

	if item.bigAccount {
		// Storage is built inline by Stage C. Pass the entity through.
		res.bigEnt = item.ent
	}

	storageRoot := emptyTrieHash

	if item.hasPreAllocRoot {
		storageRoot = item.preAllocRoot
		// No slots to process; storageRows stays nil.
	} else if !item.bigAccount && len(item.ent.slots) > 0 {
		rows, root, slotCount, slotBytes, err := buildStorageTrieBuffered(item.addrHash, item.ent, emptyTrieHash)
		if err != nil {
			res.buildErr = err
			return res
		}
		res.storageRows = rows
		storageRoot = root
		res.stats.StorageSlotsCreated = slotCount
		res.stats.StorageBytes = slotBytes
	}
	res.storageRoot = storageRoot

	// Code hash (pure; always computed here, even for big accounts, since it
	// requires no storage root knowledge).
	codeHash := emptyCodeHash
	if len(item.ent.code) > 0 {
		codeHash = crypto.Keccak256Hash(item.ent.code)
		res.stats.CodeBytes = uint64(len(item.ent.code))
	}
	res.codeHash = codeHash
	res.code = item.ent.code

	if !item.bigAccount {
		// Pre-compute accountRLP for normal accounts (storageRoot is final here).
		accountRLP := ethrexinternal.EncodeAccountState(item.ent.nonce, item.ent.balance, storageRoot, codeHash)
		res.accountRLP = accountRLP
		res.stats.AccountBytes = uint64(len(accountRLP))
		res.stats.IsContract = len(item.ent.code) != 0 || storageRoot != emptyTrieHash
	}

	return res
}

// ---------------------------------------------------------------------------
// Entity blob codec — identical shape to besu's but self-contained.
// ---------------------------------------------------------------------------

type entityKind byte

const (
	entityEOA      entityKind = 1
	entityContract entityKind = 2
)

type entity struct {
	kind    entityKind
	nonce   uint64
	balance *uint256.Int
	code    []byte
	// slots maps raw storage key (common.Hash) → value (uint256).
	// Populated from GenesisStorage[addr] and AutoFill contract storage.
	slots map[common.Hash]*uint256.Int
}

// encodeEntity serialises an entity to the streamsort blob.
//
// Format (EOA):
//
//	[0x01] [nonce u64 BE] [balance_len u8] [balance bytes]
//
// Format (contract):
//
//	[0x02] [nonce u64 BE] [balance_len u8] [balance bytes]
//	[code_len u32 BE] [code bytes]
//	[slot_count u32 BE] [slot_count × (32B key, 32B value)]
func encodeEntity(nonce uint64, balance *uint256.Int, code []byte, slots map[common.Hash]common.Hash) []byte {
	if len(code) == 0 && len(slots) == 0 {
		// EOA path.
		balBytes := balance.ToBig().Bytes()
		out := make([]byte, 1+8+1+len(balBytes))
		out[0] = byte(entityEOA)
		binary.BigEndian.PutUint64(out[1:9], nonce)
		out[9] = byte(len(balBytes))
		copy(out[10:], balBytes)
		return out
	}
	balBytes := balance.ToBig().Bytes()
	size := 1 + 8 + 1 + len(balBytes) + 4 + len(code) + 4 + len(slots)*64
	out := make([]byte, 0, size)
	out = append(out, byte(entityContract))
	var nonceBuf [8]byte
	binary.BigEndian.PutUint64(nonceBuf[:], nonce)
	out = append(out, nonceBuf[:]...)
	out = append(out, byte(len(balBytes)))
	out = append(out, balBytes...)
	var codeLenBuf [4]byte
	binary.BigEndian.PutUint32(codeLenBuf[:], uint32(len(code)))
	out = append(out, codeLenBuf[:]...)
	out = append(out, code...)
	var slotCountBuf [4]byte
	binary.BigEndian.PutUint32(slotCountBuf[:], uint32(len(slots)))
	out = append(out, slotCountBuf[:]...)
	for k, v := range slots {
		out = append(out, k[:]...)
		out = append(out, v[:]...)
	}
	return out
}

func decodeEntity(blob []byte) entity {
	if len(blob) < 1 {
		panic("ethrex: empty entity blob")
	}
	e := entity{kind: entityKind(blob[0])}
	pos := 1
	e.nonce = binary.BigEndian.Uint64(blob[pos : pos+8])
	pos += 8
	balLen := int(blob[pos])
	pos++
	balBytes := blob[pos : pos+balLen]
	pos += balLen
	e.balance = new(uint256.Int)
	e.balance.SetBytes(balBytes)

	if e.kind == entityContract {
		codeLen := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
		pos += 4
		e.code = make([]byte, codeLen)
		copy(e.code, blob[pos:pos+codeLen])
		pos += codeLen
		slotCount := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
		pos += 4
		e.slots = make(map[common.Hash]*uint256.Int, slotCount)
		for i := 0; i < slotCount; i++ {
			var k, v common.Hash
			copy(k[:], blob[pos:pos+32])
			pos += 32
			copy(v[:], blob[pos:pos+32])
			pos += 32
			u := new(uint256.Int)
			u.SetBytes32(v[:])
			e.slots[k] = u
		}
	}
	return e
}
