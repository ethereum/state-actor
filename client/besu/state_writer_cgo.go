//go:build cgo_besu

package besu

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"sort"

	"github.com/cockroachdb/pebble"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/besu"
	besurlp "github.com/nerolation/state-actor/internal/besu/rlp"
	besutrie "github.com/nerolation/state-actor/internal/besu/trie"
	"github.com/nerolation/state-actor/internal/entitygen"
)

// phase1FlushBytes caps the temp-Pebble write batch at 64 MiB to bound Phase 1
// memory while amortizing per-batch syscall overhead.
const phase1FlushBytes = 64 * 1024 * 1024

// writeStateAndCollectRoot drives the two-phase streaming pipeline.
//
// Phase 1 generates entities via a single-goroutine RNG (math/rand.Rand is
// not thread-safe) and writes (addrHash → entityBlob) to a temp Pebble DB,
// which auto-sorts by key. Phase 2 iterates the sorted DB, builds per-account
// storage tries, writes flat state and code, and feeds (addrHash, accountRLP)
// into the account trie builder. SaveWorldState is invoked here at the end.
//
// Memory bound: O(max storage slots per single contract). The full account
// set never lives in RAM at once.
func writeStateAndCollectRoot(
	ctx context.Context,
	cfg generator.Config,
	db *besuDB,
	sink *nodeSink,
) (common.Hash, []byte, *generator.Stats, error) {
	stats := &generator.Stats{}

	// --- Phase 1: stream entities to temp Pebble. ---

	tmpDir, err := os.MkdirTemp("", "besu-acct-trie-*")
	if err != nil {
		return common.Hash{}, nil, nil, fmt.Errorf("besu: mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pdb, err := pebble.Open(tmpDir, &pebble.Options{})
	if err != nil {
		return common.Hash{}, nil, nil, fmt.Errorf("besu: open temp pebble: %w", err)
	}

	rng := mrand.New(mrand.NewSource(int64(cfg.Seed)))
	pendingBytes := 0
	batch := pdb.NewBatch()
	flush := func() error {
		if err := batch.Commit(pebble.Sync); err != nil {
			return err
		}
		batch = pdb.NewBatch()
		pendingBytes = 0
		return nil
	}

	// Phase 1 target-size cap. Tracks raw entity bytes (32B addrHash key + blob)
	// and stops emission once cfg.TargetSize is reached. Over-estimates the
	// final Bonsai DB size (no trie-node overhead) but lands slightly under
	// target — preferred over going over.
	totalRawBytes := uint64(0)
	targetReached := false
	checkTarget := func(blobLen int) bool {
		totalRawBytes += uint64(32 + blobLen)
		if cfg.TargetSize > 0 && totalRawBytes >= cfg.TargetSize {
			if cfg.Verbose {
				log.Printf("besu Phase 1: raw bytes %d MiB >= target %d MiB — stopping entity emission early",
					totalRawBytes>>20, cfg.TargetSize>>20)
			}
			targetReached = true
			return true
		}
		return false
	}

	for i := 0; i < cfg.NumAccounts && !targetReached; i++ {
		if err := ctx.Err(); err != nil {
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		acc := entitygen.GenerateEOA(rng)
		addrHash := acc.AddrHash
		blob := encodeEntityEOA(acc.StateAccount.Nonce, acc.StateAccount.Balance)
		if err := batch.Set(addrHash[:], blob, nil); err != nil {
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		pendingBytes += 32 + len(blob)
		if pendingBytes >= phase1FlushBytes {
			if err := flush(); err != nil {
				pdb.Close()
				return common.Hash{}, nil, nil, err
			}
		}
		if checkTarget(len(blob)) {
			break
		}
	}

	// Inject explicitly-requested addresses (e.g. Anvil default) as EOAs with
	// 999_999_999 ETH, nonce=0, no code/storage. Mirrors generator.Generator's
	// InjectAddresses handling.
	injectBalance := new(uint256.Int).Mul(uint256.NewInt(999_999_999), uint256.NewInt(1_000_000_000_000_000_000))
	seenInjected := make(map[common.Address]struct{}, len(cfg.InjectAddresses))
	for _, addr := range cfg.InjectAddresses {
		if err := ctx.Err(); err != nil {
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		if _, dup := seenInjected[addr]; dup {
			continue
		}
		seenInjected[addr] = struct{}{}
		addrHash := crypto.Keccak256Hash(addr[:])
		// Inject as an EOA with the canonical large balance.
		blob := encodeEntityEOA(0, injectBalance)
		if err := batch.Set(addrHash[:], blob, nil); err != nil {
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		pendingBytes += 32 + len(blob)
		if pendingBytes >= phase1FlushBytes {
			if err := flush(); err != nil {
				pdb.Close()
				return common.Hash{}, nil, nil, err
			}
		}
	}

	codeSize := cfg.CodeSize
	if codeSize <= 0 {
		codeSize = 1024
	}
	for i := 0; i < cfg.NumContracts && !targetReached; i++ {
		if err := ctx.Err(); err != nil {
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		// Canonical (slot-count, contract) draw order — single source of
		// truth in entitygen so every writer + every reproduction-side
		// test stays RNG-aligned.
		contract := entitygen.GenerateContractRoll(rng, cfg.Distribution, codeSize, cfg.MinSlots, cfg.MaxSlots)
		slotMap := make(map[common.Hash]common.Hash, len(contract.Storage))
		for _, s := range contract.Storage {
			slotMap[s.Key] = s.Value
		}
		addrHash := contract.AddrHash
		blob := encodeEntityContract(contract.StateAccount.Nonce, contract.StateAccount.Balance, contract.Code, slotMap)
		if err := batch.Set(addrHash[:], blob, nil); err != nil {
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		pendingBytes += 32 + len(blob)
		if pendingBytes >= phase1FlushBytes {
			if err := flush(); err != nil {
				pdb.Close()
				return common.Hash{}, nil, nil, err
			}
		}
		if checkTarget(len(blob)) {
			break
		}
	}
	if err := flush(); err != nil {
		pdb.Close()
		return common.Hash{}, nil, nil, err
	}
	// Compact merges all SSTs into one sorted run for fastest forward iteration.
	if err := pdb.Compact(nil, []byte{0xff, 0xff, 0xff, 0xff}, false); err != nil {
		pdb.Close()
		return common.Hash{}, nil, nil, err
	}

	// --- Phase 2: iterate sorted, drive Builder + flat-state writes. ---

	builder := besutrie.New(sink)
	iter, err := pdb.NewIter(nil)
	if err != nil {
		pdb.Close()
		return common.Hash{}, nil, nil, fmt.Errorf("besu: temp iter: %w", err)
	}
	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			iter.Close()
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		var addrHash common.Hash
		copy(addrHash[:], iter.Key())
		entity := decodeEntity(iter.Value())

		// Storage trie + flat slots + code.
		storageRoot := besu.EmptyTrieNodeHash
		codeHash := besu.EmptyCodeHash
		if entity.kind == entityContract {
			if len(entity.slots) > 0 {
				sb := builder.BeginStorage(addrHash)
				// Sort slots by slotHash (storage trie also requires sorted insert).
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
					valueRLP := besurlp.EncodeStorageValue(e.value)
					if err := sb.AddSlot(e.slotHash, valueRLP); err != nil {
						iter.Close()
						pdb.Close()
						return common.Hash{}, nil, nil, err
					}
					if err := sink.PutFlatStorage(addrHash, e.slotHash, valueRLP); err != nil {
						iter.Close()
						pdb.Close()
						return common.Hash{}, nil, nil, err
					}
					stats.StorageSlotsCreated++
					stats.StorageBytes += uint64(64 + len(valueRLP))
				}
				root, err := sb.Commit()
				if err != nil {
					iter.Close()
					pdb.Close()
					return common.Hash{}, nil, nil, err
				}
				storageRoot = root
			}
			if len(entity.code) > 0 {
				codeHash = crypto.Keccak256Hash(entity.code)
				if err := sink.PutCode(codeHash, entity.code); err != nil {
					iter.Close()
					pdb.Close()
					return common.Hash{}, nil, nil, err
				}
				stats.CodeBytes += uint64(len(entity.code))
			}
		}

		accountRLP, err := besurlp.EncodeAccount(entity.nonce, entity.balance, storageRoot, codeHash)
		if err != nil {
			iter.Close()
			pdb.Close()
			return common.Hash{}, nil, nil, fmt.Errorf("besu: encode account: %w", err)
		}
		if err := sink.PutFlatAccount(addrHash, accountRLP); err != nil {
			iter.Close()
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}
		if err := builder.AddAccount(addrHash, accountRLP); err != nil {
			iter.Close()
			pdb.Close()
			return common.Hash{}, nil, nil, err
		}

		if entity.kind == entityEOA {
			stats.AccountsCreated++
		} else {
			stats.ContractsCreated++
		}
		stats.AccountBytes += uint64(32 + len(accountRLP))
	}
	if err := iter.Close(); err != nil {
		pdb.Close()
		return common.Hash{}, nil, nil, err
	}
	if err := pdb.Close(); err != nil {
		return common.Hash{}, nil, nil, err
	}

	// Commit the account trie. NodeSink emits remaining trie nodes.
	rootHash, rootRLP, err := builder.Commit()
	if err != nil {
		return common.Hash{}, nil, nil, fmt.Errorf("besu: trie commit: %w", err)
	}

	stats.TotalBytes = stats.AccountBytes + stats.StorageBytes + stats.CodeBytes
	return rootHash, rootRLP, stats, nil
}

// --- Entity types and encoding ---

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

// --- Entity blob serialization for temp Pebble ---
//
// Format (single-byte kind tag + fields):
//
//   EOA:
//     [0x01] [nonce u64 BE] [balance_len u8] [balance bytes...]
//
//   Contract:
//     [0x02] [nonce u64 BE] [balance_len u8] [balance bytes...]
//        [code_len u32 BE] [code bytes...]
//        [slot_count u32 BE] [slot_count × ([slot_key 32B] [slot_value 32B])]

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
