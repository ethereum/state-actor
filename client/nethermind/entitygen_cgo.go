//go:build cgo_neth

package nethermind

import (
	"bytes"
	"context"
	"fmt"
	mrand "math/rand"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"

	"github.com/nerolation/state-actor/generator"
	nethtrie "github.com/nerolation/state-actor/internal/neth/trie"
	"github.com/nerolation/state-actor/internal/streamsort"
)

// maxPhase0Workers caps Phase 0 drain-and-compute parallelism. Each worker
// holds a nethtrie.Builder + stateDBSink (grocksdb.WriteBatch up to
// stateBatchFlushBytes) + a streamsort.Store; peak per-worker RAM scales
// linearly.
const maxPhase0Workers = 8

// writeSyntheticAccounts populates the State + Code DBs from synthetic
// EOAs/contracts, genesis-alloc entries, and spec-PreAlloc entities,
// and returns the computed state root.
//
// Phase 0 streams each spec-PreAlloc entity's storage into the builder
// (BeginStorage→AddStorageSlot→FinalizeStorageRoot) and splices the
// returned root into cfg.GenesisAccounts[addr].Root.
//
// Phase 1 generates entitygen output, drives per-contract storage tries
// against the State DB at HalfPath storage keys, and queues
// (addrHash → StateAccount blob) into a streamsort.Store.
//
// Phase 2 iterates the streamsort.Store in addrHash order, decodes the
// stashed StateAccount, encodes Nethermind RLP, and drives
// Builder.AddAccount. FinalizeStateRoot returns the final root.
//
// Memory: O(max slots per contract).
func writeSyntheticAccounts(
	ctx context.Context,
	dbs *nethDBs,
	cfg generator.Config,
	genesisAccounts map[common.Address]*types.StateAccount,
	genesisCodes map[common.Address][]byte,
	genesisStorages map[common.Address]map[common.Hash]common.Hash,
	stats *generator.Stats,
) (common.Hash, error) {
	sink := newStateDBSink(dbs.state)
	defer func() { _ = sink.close() }()
	builder := nethtrie.NewBuilder(sink)

	sorter, err := streamsort.New("")
	if err != nil {
		return common.Hash{}, fmt.Errorf("open streamsort: %w", err)
	}
	defer sorter.Close()

	codeSink := newCodeDBSink(dbs.code)
	defer func() { _ = codeSink.close() }()

	// Phase 0: drain every spec-PreAlloc entity's storage trie in parallel.
	// Workers each own (a) a stateDBSink wrapping a per-worker
	// grocksdb.WriteBatch — grocksdb's Write is safe to call concurrently
	// across workers per RocksDB's WAL serialization (besu commit 4847945,
	// docs and matching pattern at client/besu/state_writer_cgo.go:308-432),
	// and (b) a per-worker nethtrie.Builder so the single-goroutine
	// invariant (internal/neth/trie/builder.go:60-61) holds within each
	// worker. Storage roots are content-addressed (keccak), so out-of-order
	// completion doesn't affect determinism; the main goroutine assigns
	// roots into genesisAccounts in receive order — the eventual state-trie
	// root depends only on sorted addrHash iteration (sorter.Iterate below).
	//
	// Long-pole scheduling: bloatnet specs have 5 EOAs with 100M-1B storage
	// slots each. FIFO would put one bloat on a worker mid-run, making it
	// the wall-clock floor. Sort indices descending by len(pe.Storage) to
	// launch the largest entities at t=0 across the first workers.
	if err := runPhase0(ctx, cfg, dbs, genesisAccounts, stats); err != nil {
		return common.Hash{}, fmt.Errorf("nethermind: phase 0: %w", err)
	}

	// Genesis-alloc accounts go to the temp DB AND the code DB AND
	// (for storage-bearing accounts) into the per-account storage-trie
	// path that produces account.Root before the account RLP is queued.
	// Storage slots flow through the standard storage-trie path:
	// keccak(slotKey)-sorted slots driven through builder.AddStorageSlot,
	// then FinalizeStorageRoot stamps account.Root.
	for addr, acc := range genesisAccounts {
		if code, ok := genesisCodes[addr]; ok && len(code) > 0 {
			ch := crypto.Keccak256Hash(code)
			if err := codeSink.put(ch[:], code); err != nil {
				return common.Hash{}, fmt.Errorf("write genesis code for %s: %w", addr.Hex(), err)
			}
			acc.CodeHash = ch[:]
			if stats != nil {
				stats.CodeBytes += uint64(len(code))
			}
		}

		// Storage trie: slots enter the StackTrie in keccak(slotKey)-
		// ascending order; values are leading-zero-trimmed + RLP-encoded.
		if slots := genesisStorages[addr]; len(slots) > 0 {
			ahBytes := crypto.Keccak256Hash(addr[:])
			var ah [32]byte
			copy(ah[:], ahBytes[:])

			type hashedSlot struct {
				keyHash common.Hash
				value   common.Hash
			}
			hashed := make([]hashedSlot, 0, len(slots))
			for k, v := range slots {
				hashed = append(hashed, hashedSlot{
					keyHash: crypto.Keccak256Hash(k[:]),
					value:   v,
				})
			}
			sort.Slice(hashed, func(i, j int) bool {
				return bytes.Compare(hashed[i].keyHash[:], hashed[j].keyHash[:]) < 0
			})
			for _, s := range hashed {
				v := s.value[:]
				for len(v) > 0 && v[0] == 0 {
					v = v[1:]
				}
				if len(v) == 0 {
					continue // zero slot = deletion; skip
				}
				valRLP, err := gethrlp.EncodeToBytes(v)
				if err != nil {
					return common.Hash{}, fmt.Errorf("encode genesis-alloc slot %s/%s: %w",
						addr.Hex(), s.keyHash.Hex(), err)
				}
				if err := builder.AddStorageSlot(ah, [32]byte(s.keyHash), valRLP); err != nil {
					return common.Hash{}, fmt.Errorf("add genesis-alloc storage slot %s/%s: %w",
						addr.Hex(), s.keyHash.Hex(), err)
				}
				if stats != nil {
					stats.StorageBytes += uint64(len(valRLP))
				}
			}
			storageRoot, err := builder.FinalizeStorageRoot(ah)
			if err != nil {
				return common.Hash{}, fmt.Errorf("finalize genesis-alloc storage root for %s: %w",
					addr.Hex(), err)
			}
			acc.Root = common.Hash(storageRoot)
		}

		ah := crypto.Keccak256Hash(addr[:])
		data, err := gethrlp.EncodeToBytes(acc)
		if err != nil {
			return common.Hash{}, fmt.Errorf("encode genesis account %s: %w", addr.Hex(), err)
		}
		if err := sorter.Put(ah[:], data); err != nil {
			return common.Hash{}, fmt.Errorf("queue genesis account: %w", err)
		}
		if stats != nil {
			stats.AccountBytes += uint64(len(data))
		}
	}

	rng := mrand.New(mrand.NewSource(cfg.Seed))

	plan := cfg.AutoFill

	if plan != nil {
		for i := 0; i < plan.NumEOAs; i++ {
			acc := plan.DrawEOA(rng)
			data, err := gethrlp.EncodeToBytes(acc.StateAccount)
			if err != nil {
				return common.Hash{}, fmt.Errorf("encode EOA %d: %w", i, err)
			}
			if err := sorter.Put(acc.AddrHash[:], data); err != nil {
				return common.Hash{}, fmt.Errorf("queue EOA: %w", err)
			}
			if stats != nil {
				stats.AccountBytes += uint64(len(data))
				stats.AccountsCreated++
			}
			if len(acc.Code) > 0 {
				if err := codeSink.put(acc.CodeHash[:], acc.Code); err != nil {
					return common.Hash{}, fmt.Errorf("write EOA delegation code: %w", err)
				}
				if stats != nil {
					stats.CodeBytes += uint64(len(acc.Code))
				}
			}
		}
	}

	plannedContracts := 0
	if plan != nil {
		plannedContracts = plan.NumContracts
	}
	for i := 0; i < plannedContracts; i++ {
		contract := plan.DrawContract(rng)
		numSlots := len(contract.Storage)

		if err := codeSink.put(contract.CodeHash[:], contract.Code); err != nil {
			return common.Hash{}, fmt.Errorf("write contract code: %w", err)
		}
		if stats != nil {
			stats.CodeBytes += uint64(len(contract.Code))
		}

		// AddStorageSlot requires slotKeyHash-ascending order; re-hash
		// and re-sort since entitygen sorted by raw Key.
		if numSlots > 0 {
			slots := make([]hashedSlot, len(contract.Storage))
			for j, s := range contract.Storage {
				slots[j] = hashedSlot{
					keyHash: crypto.Keccak256Hash(s.Key[:]),
					value:   s.Value,
				}
			}
			sort.Slice(slots, func(i, j int) bool {
				return bytes.Compare(slots[i].keyHash[:], slots[j].keyHash[:]) < 0
			})

			for _, s := range slots {
				valueRLP, err := encodeStorageValueNeth(s.value)
				if err != nil {
					return common.Hash{}, fmt.Errorf("encode slot: %w", err)
				}
				if valueRLP == nil {
					continue
				}
				if err := builder.AddStorageSlot([32]byte(contract.AddrHash), [32]byte(s.keyHash), valueRLP); err != nil {
					return common.Hash{}, fmt.Errorf("add storage slot: %w", err)
				}
				if stats != nil {
					stats.StorageBytes += uint64(len(valueRLP))
				}
			}
			storageRoot, err := builder.FinalizeStorageRoot([32]byte(contract.AddrHash))
			if err != nil {
				return common.Hash{}, fmt.Errorf("finalize storage root: %w", err)
			}
			contract.StateAccount.Root = common.Hash(storageRoot)
		}

		data, err := gethrlp.EncodeToBytes(contract.StateAccount)
		if err != nil {
			return common.Hash{}, fmt.Errorf("encode contract %d: %w", i, err)
		}
		if err := sorter.Put(contract.AddrHash[:], data); err != nil {
			return common.Hash{}, fmt.Errorf("queue contract: %w", err)
		}
		if stats != nil {
			stats.AccountBytes += uint64(len(data))
			stats.ContractsCreated++
			stats.StorageSlotsCreated += numSlots
		}
	}

	if err := sorter.Iterate(func(key, value []byte) error {
		var ah [32]byte
		copy(ah[:], key)

		// Skip the decode + re-encode round-trip: nethrlp.EncodeAccount
		// (internal/neth/rlp/account.go:28-30) is literally
		// gethrlp.EncodeToBytes(acc), which is the same encoder that
		// produced the stashed `value` bytes in the loops above. Saves
		// ~5-10% of Phase 2 wall on the 215K-entity bloatnet workload.
		// Determinism preserved: the bytes ARE the canonical Nethermind
		// RLP encoding by construction.
		if err := builder.AddAccount(ah, value); err != nil {
			return fmt.Errorf("add account: %w", err)
		}
		return nil
	}); err != nil {
		return common.Hash{}, fmt.Errorf("streamsort iterate: %w", err)
	}

	root, err := builder.FinalizeStateRoot()
	if err != nil {
		return common.Hash{}, fmt.Errorf("finalize state root: %w", err)
	}
	// Flush before return so the genesis-block writer sees a coherent
	// State DB and so failures surface synchronously.
	if err := sink.close(); err != nil {
		return common.Hash{}, fmt.Errorf("flush state writes: %w", err)
	}
	return common.Hash(root), nil
}

// encodeStorageValueNeth RLP-encodes a storage slot value with leading
// zeros trimmed — the same wire format Nethermind reads. Returns nil for
// the all-zero hash (which represents a deletion in MPT semantics).
func encodeStorageValueNeth(value common.Hash) ([]byte, error) {
	v := value[:]
	for len(v) > 0 && v[0] == 0 {
		v = v[1:]
	}
	if len(v) == 0 {
		return nil, nil
	}
	return gethrlp.EncodeToBytes(v)
}

type hashedSlot struct {
	keyHash common.Hash
	value   common.Hash
}

// nethermindStorageHashBuilder adapts nethtrie.Builder to
// streamingtrie.HashBuilder. AddStorageSlot writes both the per-slot
// Storage CF row and advances the storage trie, so the streamingtrie
// Sink slot is left nil for nethermind.
type nethermindStorageHashBuilder struct {
	builder *nethtrie.Builder
	ah      [32]byte
}

func (n *nethermindStorageHashBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	var kh [32]byte
	copy(kh[:], keyHash[:])
	return n.builder.AddStorageSlot(n.ah, kh, valueRLP)
}

func (n *nethermindStorageHashBuilder) Root() (common.Hash, error) {
	root, err := n.builder.FinalizeStorageRoot(n.ah)
	if err != nil {
		return common.Hash{}, err
	}
	return common.Hash(root), nil
}
