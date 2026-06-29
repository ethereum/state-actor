//go:build cgo_besu

package besu

import (
	"context"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"runtime"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/besu"
	besurlp "github.com/ethereum/state-actor/internal/besu/rlp"
	besutrie "github.com/ethereum/state-actor/internal/besu/trie"
	"github.com/ethereum/state-actor/internal/streamingtrie"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// maxPhase0Workers caps Phase 0 drain-and-compute parallelism so peak
// per-worker RAM (streamsort.Store + grocksdb.WriteBatch) stays within budget.
const maxPhase0Workers = 8

// writeStateAndCollectRoot drives the two-phase streaming pipeline:
// Phase 0 streams spec-PreAlloc storage into Bonsai per-account storage
// tries and flat-state. Phase 1 streams synthetic + genesis-alloc
// entities into a streamsort.Store keyed by addrHash. Phase 2 iterates
// sorted, builds per-account storage tries, writes flat state and
// code, and feeds the outer account trie.
//
// Memory bound: O(max storage slots in any single contract).
func writeStateAndCollectRoot(
	ctx context.Context,
	cfg generator.Config,
	db *besuDB,
	sink *nodeSink,
) (common.Hash, []byte, *generator.Stats, error) {
	stats := &generator.Stats{}

	// builder streams the account-state trie: it emits trie nodes as the
	// right-spine collapses (O(trie depth) memory) rather than holding the
	// entire account MPT resident until Commit (O(accounts) — which OOMs on
	// large datadirs). Fed in Phase 2 in sorted addrHash order, matching how
	// geth/reth/neth build their account trie via StackTrie / HashBuilder.
	builder := besutrie.NewStreamingAccountBuilder(sink)

	// hashToAddr lets Phase 2 read cfg.GenesisAccounts[addr].Root (set
	// by Phase 0) when encoding spec-entity account leaves.
	hashToAddr := make(map[common.Hash]common.Address, len(cfg.GenesisAccounts))
	for addr := range cfg.GenesisAccounts {
		hashToAddr[crypto.Keccak256Hash(addr[:])] = addr
	}

	if err := runPhase0(ctx, cfg, db, stats); err != nil {
		return common.Hash{}, nil, nil, err
	}

	sorter, err := streamsort.New("")
	if err != nil {
		return common.Hash{}, nil, nil, fmt.Errorf("besu: streamsort.New: %w", err)
	}
	defer sorter.Close()

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))

	plan := cfg.AutoFill
	if plan != nil {
		cfg.Progress.Stage("besu: phase 1/2 — generating accounts")
		for i := 0; i < plan.NumEOAs; i++ {
			if err := ctx.Err(); err != nil {
				return common.Hash{}, nil, nil, err
			}
			acc := plan.DrawEOA(rng)
			addrHash := acc.AddrHash
			var blob []byte
			if len(acc.Code) > 0 {
				blob = encodeEntityContract(acc.StateAccount.Nonce, acc.StateAccount.Balance, acc.Code, nil)
			} else {
				blob = encodeEntityEOA(acc.StateAccount.Nonce, acc.StateAccount.Balance)
			}
			if err := sorter.Put(addrHash[:], blob); err != nil {
				return common.Hash{}, nil, nil, err
			}
			cfg.Progress.Tick(int64(i+1), int64(plan.NumEOAs), "EOAs")
		}
	}

	seenAlloc := make(map[common.Address]struct{}, len(cfg.GenesisAccounts))
	for addr, acc := range cfg.GenesisAccounts {
		if err := ctx.Err(); err != nil {
			return common.Hash{}, nil, nil, err
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
		var blob []byte
		if len(code) == 0 && len(cfg.GenesisStorage[addr]) == 0 {
			blob = encodeEntityEOA(acc.Nonce, balance)
		} else {
			blob = encodeEntityContract(acc.Nonce, balance, code, cfg.GenesisStorage[addr])
		}
		if err := sorter.Put(addrHash[:], blob); err != nil {
			return common.Hash{}, nil, nil, err
		}
	}

	if plan != nil {
		cfg.Progress.Stage("besu: phase 1/2 — generating contracts")
		for i := 0; i < plan.NumContracts; i++ {
			if err := ctx.Err(); err != nil {
				return common.Hash{}, nil, nil, err
			}
			contract := plan.DrawContract(rng)
			slotMap := make(map[common.Hash]common.Hash, len(contract.Storage))
			for _, s := range contract.Storage {
				slotMap[s.Key] = s.Value
			}
			addrHash := contract.AddrHash
			blob := encodeEntityContract(contract.StateAccount.Nonce, contract.StateAccount.Balance, contract.Code, slotMap)
			if err := sorter.Put(addrHash[:], blob); err != nil {
				return common.Hash{}, nil, nil, err
			}
			cfg.Progress.Tick(int64(i+1), int64(plan.NumContracts), "contracts")
		}
	}

	// --- Phase 2: iterate sorted, drive Builder + flat-state writes. ---
	// entitiesQueued is the exact count Put into the sorter above (synthetic
	// EOAs/contracts + deduped genesis allocs) — the Phase-2 progress total.
	var entitiesQueued int64
	if plan != nil {
		entitiesQueued += int64(plan.NumEOAs + plan.NumContracts)
	}
	entitiesQueued += int64(len(seenAlloc))
	cfg.Progress.Stage("besu: phase 2/2 — building state trie")
	var phase2Count int64
	if err := sorter.Iterate(func(key, value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var addrHash common.Hash
		copy(addrHash[:], key)
		entity := decodeEntity(value)

		// Phase 0 may have set Root on the spec entity already.
		storageRoot := besu.EmptyTrieNodeHash
		if specAddr, ok := hashToAddr[addrHash]; ok {
			if acc := cfg.GenesisAccounts[specAddr]; acc != nil &&
				acc.Root != (common.Hash{}) && acc.Root != besu.EmptyTrieNodeHash {
				storageRoot = acc.Root
			}
		}
		codeHash := besu.EmptyCodeHash
		if entity.kind == entityContract {
			if len(entity.slots) > 0 {
				// Streaming storage builder: O(depth) like the account trie.
				// Fed below in sorted-by-slotHash order (slot hashes are unique
				// within a contract, so insertion is strictly ascending).
				sb := besutrie.NewStreamingStorageBuilder(sink, addrHash)
				// Storage trie requires sorted-by-slotHash insertion.
				type kv struct {
					slotHash common.Hash
					value    common.Hash
				}
				kvs := make([]kv, 0, len(entity.slots))
				for slotKey, slotVal := range entity.slots {
					kvs = append(kvs, kv{
						slotHash: crypto.Keccak256Hash(slotKey[:]),
						value:    slotVal,
					})
				}
				sort.Slice(kvs, func(i, j int) bool {
					return kvs[i].slotHash.Big().Cmp(kvs[j].slotHash.Big()) < 0
				})
				for _, e := range kvs {
					// Trie value: RLP(trim(value)) (≤33 B).
					// Flat value: raw trim(value) (≤32 B; UInt256.fromBytes
					// rejects RLP-wrapped values).
					valueTrieRLP := besurlp.EncodeStorageValue(e.value)
					valueFlat := besurlp.TrimStorageValue(e.value)
					if err := sb.AddSlot(e.slotHash, valueTrieRLP); err != nil {
						return err
					}
					if err := sink.PutFlatStorage(addrHash, e.slotHash, valueFlat); err != nil {
						return err
					}
					stats.StorageSlotsCreated++
					stats.StorageBytes += uint64(64 + len(valueTrieRLP) + len(valueFlat))
				}
				root, err := sb.Commit()
				if err != nil {
					return err
				}
				storageRoot = root
			}
			if len(entity.code) > 0 {
				codeHash = crypto.Keccak256Hash(entity.code)
				if err := sink.PutCode(codeHash, entity.code); err != nil {
					return err
				}
				stats.CodeBytes += uint64(len(entity.code))
			}
		}

		accountRLP, err := besurlp.EncodeAccount(entity.nonce, entity.balance, storageRoot, codeHash)
		if err != nil {
			return fmt.Errorf("besu: encode account: %w", err)
		}
		if err := sink.PutFlatAccount(addrHash, accountRLP); err != nil {
			return err
		}
		if err := builder.AddAccount(addrHash, accountRLP); err != nil {
			return err
		}

		if entity.kind == entityEOA {
			stats.AccountsCreated++
		} else {
			stats.ContractsCreated++
		}
		stats.AccountBytes += uint64(32 + len(accountRLP))
		phase2Count++
		cfg.Progress.Tick(phase2Count, entitiesQueued, "accounts")
		return nil
	}); err != nil {
		return common.Hash{}, nil, nil, err
	}

	rootHash, rootRLP, err := builder.Commit()
	if err != nil {
		return common.Hash{}, nil, nil, fmt.Errorf("besu: trie commit: %w", err)
	}

	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return rootHash, rootRLP, stats, nil
}

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
	slots   map[common.Hash]common.Hash
}

// Entity blob format:
//
//   EOA:      [0x01] [nonce u64 BE] [balance_len u8] [balance bytes]
//   Contract: [0x02] [nonce u64 BE] [balance_len u8] [balance bytes]
//             [code_len u32 BE] [code bytes]
//             [slot_count u32 BE] [slot_count × (32B key, 32B value)]

func encodeEntityEOA(nonce uint64, balance *uint256.Int) []byte {
	balBytes := balance.ToBig().Bytes() // minimal big-endian
	out := make([]byte, 1+8+1+len(balBytes))
	out[0] = byte(entityEOA)
	binary.BigEndian.PutUint64(out[1:9], nonce)
	out[9] = byte(len(balBytes))
	copy(out[10:], balBytes)
	return out
}

func encodeEntityContract(nonce uint64, balance *uint256.Int, code []byte, slots map[common.Hash]common.Hash) []byte {
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

// runPhase0 drives the spec-PreAlloc streaming-trie phase in parallel.
//
// For every entity with pe.Storage != nil, a worker:
//  1. Owns its own *nodeSink wrapping a per-worker *grocksdb.WriteBatch.
//     Flushes at flushThresholdBytes (64 MiB) via db.db.Write — Pebble's
//     WAL is disabled on the bulk path, and grocksdb's Write is safe to
//     call concurrently across workers (RocksDB serialises the commit
//     pipeline internally).
//  2. Constructs its own besutrie.StreamingStorageBuilder via
//     NewStreamingStorageBuilder so trie-node writes route through the
//     per-worker sink, not the shared outer-builder sink.
//  3. Calls streamingtrie.StorageRoot to drain the iter into a per-call
//     streamsort.Store (rooted under cfg.DBPath, not /tmp), walk it
//     sorted, drive the streaming builder, and emit flat-state writes
//     via a closure that calls workerSink.PutFlatStorage.
//  4. Reports the computed storage root to the main goroutine via
//     preparedCh; main assigns cfg.GenesisAccounts[addr].Root.
//
// Across-entity Bonsai keyspaces are disjoint (every key prefixed by
// addrHash for trie + flat), so parallel writes don't conflict.
func runPhase0(ctx context.Context, cfg generator.Config, db *besuDB, stats *generator.Stats) error {
	indices := make([]int, 0, len(cfg.PreAlloc))
	for i := range cfg.PreAlloc {
		if cfg.PreAlloc[i].Storage != nil {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return nil
	}

	// Phase 0 streams each spec entity's storage slots and is otherwise silent
	// for hours on bloat specs. The count-only slot heartbeat funnels every
	// worker's per-slot count through one SlotMeter; each worker holds its own
	// SlotWorker so the hot per-slot path stays cheap (one non-atomic add + mask).
	cfg.Progress.Stage("besu: phase 0 — spec storage")
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
		idx          int
		addr         common.Address
		root         common.Hash
		storageBytes uint64
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
			workerSink := newNodeSink(db)
			defer func() {
				// Best-effort flush at worker exit. Errors surface through
				// drainCtx; the main goroutine returns the cause.
				if err := workerSink.Close(); err != nil {
					cancelDrain(fmt.Errorf("besu phase0: worker sink close: %w", err))
				}
			}()

			for i := range drainCh {
				if drainCtx.Err() != nil {
					return
				}
				pe := &cfg.PreAlloc[i]
				addr := pe.Address
				addrHash := crypto.Keccak256Hash(addr[:])

				sb := besutrie.NewStreamingStorageBuilder(workerSink, addrHash)
				var entityStorageBytes uint64
				streamSink := func(keyHash, _rawKey, value common.Hash) error {
					slotW.Slot()
					trimmed := besurlp.TrimStorageValue(value)
					entityStorageBytes += uint64(len(trimmed))
					return workerSink.PutFlatStorage(addrHash, keyHash, trimmed)
				}
				root, err := streamingtrie.StorageRoot(cfg.DBPath, pe.Storage, sb, streamSink)
				if err != nil {
					cancelDrain(fmt.Errorf("besu: stream spec storage[%d] %s: %w", i, addr.Hex(), err))
					return
				}
				select {
				case preparedCh <- preparedEntity{idx: i, addr: addr, root: root, storageBytes: entityStorageBytes}:
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
		stats.StorageBytes += entry.storageBytes
	}
	if cause := context.Cause(drainCtx); cause != nil && cause != context.Canceled {
		return cause
	}
	return nil
}

func decodeEntity(blob []byte) entity {
	if len(blob) < 1 {
		panic("besu: empty entity blob")
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
		e.slots = make(map[common.Hash]common.Hash, slotCount)
		for i := 0; i < slotCount; i++ {
			var k, v common.Hash
			copy(k[:], blob[pos:pos+32])
			pos += 32
			copy(v[:], blob[pos:pos+32])
			pos += 32
			e.slots[k] = v
		}
	}
	return e
}
