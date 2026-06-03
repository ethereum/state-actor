//go:build cgo_ethrex

package ethrex

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"sort"

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
//  3. Iterate sorted: build per-account storage tries, write code CFs,
//     feed the account trie builder.
//
// RAM bound: internal/ethrex.Builder accumulates all leaves in memory. This
// is correct for the e2e fixture and moderate --target-size. See doc.go for
// the ceiling note.
func writeState(
	ctx context.Context,
	cfg generator.Config,
	db *ethrexDB,
	accountSink *batchSink,
	storageSink *batchSink,
) (common.Hash, *generator.Stats, error) {
	stats := &generator.Stats{}

	emptyCodeHash := common.HexToHash(ethrexinternal.EmptyCodeHashHex)
	emptyTrieHash := common.HexToHash(ethrexinternal.EmptyTrieHashHex)

	// seenCodeHash deduplicates account_codes + account_code_metadata writes.
	seenCodeHash := make(map[common.Hash]struct{})

	// writeCode writes code for a given codeHash if not already seen.
	// Always writes even for empty code (per SPIKE_FINDINGS).
	writeCode := func(codeHash common.Hash, code []byte) error {
		if _, seen := seenCodeHash[codeHash]; seen {
			return nil
		}
		seenCodeHash[codeHash] = struct{}{}
		encoded := ethrexinternal.EncodeCode(code)
		if err := db.put(cfIdxAccountCodes, codeHash[:], encoded); err != nil {
			return fmt.Errorf("ethrex: put account_codes: %w", err)
		}
		meta := ethrexinternal.CodeLengthMetadata(code)
		if err := db.put(cfIdxAccountCodeMetadata, codeHash[:], meta[:]); err != nil {
			return fmt.Errorf("ethrex: put account_code_metadata: %w", err)
		}
		return nil
	}

	// Write empty-code entry up-front (every account needs it written once).
	if err := writeCode(emptyCodeHash, nil); err != nil {
		return common.Hash{}, nil, err
	}

	// accountTrieNodeSink wraps the batchSink into a NodeSink for the account trie.
	accountTrieNodeSink := ethrexinternal.NodeSink(func(path []byte, value []byte) error {
		return accountSink.put(path, value)
	})

	// storageTrieNodeSink wraps the batchSink into a NodeSink for storage tries.
	storageTrieNodeSink := ethrexinternal.NodeSink(func(path []byte, value []byte) error {
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

	// Phase 2: iterate sorted, build per-account storage tries, write code
	// CFs, feed the outer account trie builder.
	accountBuilder := ethrexinternal.NewBuilder(accountTrieNodeSink)

	if err := sorter.Iterate(func(key, value []byte) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var addrHash common.Hash
		copy(addrHash[:], key)
		ent := decodeEntity(value)

		// Storage root: from PreAlloc pre-computed map, or built here for
		// GenesisAccounts-supplied storage, or EMPTY_TRIE_HASH.
		storageRoot := emptyTrieHash
		if preRoot, ok := preAllocStorageRoots[addrHash]; ok {
			storageRoot = preRoot
		} else if len(ent.slots) > 0 {
			type kv struct {
				slotHash common.Hash
				value    *uint256.Int
			}
			kvs := make([]kv, 0, len(ent.slots))
			for slotKey, slotVal := range ent.slots {
				slotHash := crypto.Keccak256Hash(slotKey[:])
				if slotVal.IsZero() {
					continue
				}
				kvs = append(kvs, kv{slotHash: slotHash, value: slotVal.Clone()})
			}
			if len(kvs) > 0 {
				sort.Slice(kvs, func(i, j int) bool {
					return bytes.Compare(kvs[i].slotHash[:], kvs[j].slotHash[:]) < 0
				})
				prefixedSink := ethrexinternal.PrefixedSink(addrHash, storageTrieNodeSink)
				sb := ethrexinternal.NewBuilder(prefixedSink)
				for _, e := range kvs {
					enc := ethrexinternal.EncodeStorageValue(e.value)
					if err := sb.AddLeaf(ethrexinternal.BytesToNibbles(e.slotHash[:]), enc); err != nil {
						return fmt.Errorf("ethrex: storage leaf: %w", err)
					}
					stats.StorageSlotsCreated++
					stats.StorageBytes += uint64(len(enc))
				}
				root, err := sb.Root()
				if err != nil {
					return fmt.Errorf("ethrex: storage root: %w", err)
				}
				storageRoot = root
			}
		}

		// Code hash.
		codeHash := emptyCodeHash
		if len(ent.code) > 0 {
			codeHash = crypto.Keccak256Hash(ent.code)
			if err := writeCode(codeHash, ent.code); err != nil {
				return err
			}
			stats.CodeBytes += uint64(len(ent.code))
		}

		// Account state RLP.
		accountRLP := ethrexinternal.EncodeAccountState(ent.nonce, ent.balance, storageRoot, codeHash)
		if err := accountBuilder.AddLeaf(ethrexinternal.BytesToNibbles(addrHash[:]), accountRLP); err != nil {
			return fmt.Errorf("ethrex: account leaf: %w", err)
		}
		stats.AccountBytes += uint64(len(accountRLP))

		// Classify by code + actual storage root: a PreAlloc contract's storage
		// is built in Phase 0, so ent.slots is empty here even though it has a
		// non-empty storage trie. storageRoot reflects PreAlloc + locally-built.
		if len(ent.code) == 0 && storageRoot == emptyTrieHash {
			stats.AccountsCreated++
		} else {
			stats.ContractsCreated++
		}
		return nil
	}); err != nil {
		return common.Hash{}, nil, err
	}

	// Finalize account trie.
	stateRoot, err := accountBuilder.Root()
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("ethrex: account trie root: %w", err)
	}

	// Flush trie sinks.
	if err := accountSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}
	if err := storageSink.flushSync(); err != nil {
		return common.Hash{}, nil, err
	}

	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return stateRoot, stats, nil
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
