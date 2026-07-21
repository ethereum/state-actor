//go:build cgo_neth

package nethermind

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/neth/flat"
	nethrlp "github.com/ethereum/state-actor/internal/neth/rlp"
	nethtrie "github.com/ethereum/state-actor/internal/neth/trie"
)

// newFlatStateWriter builds the flat-state trie writer and returns it with a
// close function that flushes and releases the flat sink. The trie nodes land
// in the flat DB's four node CFs, and the account and storage leaf rows are
// teed into the flat Account/Storage CFs.
func newFlatStateWriter(dbs *nethDBs) (*flatStateWriter, func() error) {
	fs := newFlatSink(dbs)
	builder := nethtrie.NewBuilder(fs)
	return &flatStateWriter{b: builder, fs: fs}, fs.close
}

// flatStateWriter tees the flat Account/Storage leaf rows before delegating to
// the underlying Builder, which computes the state root and emits the trie
// nodes into the flat node CFs (through the flatSink it was constructed with).
type flatStateWriter struct {
	b  *nethtrie.Builder
	fs *flatSink
}

func (w *flatStateWriter) AddStorageSlot(addrHash [32]byte, slotKeyHash [32]byte, valueRLP []byte) error {
	// valueRLP is already the RLP(trimmed) form — byte-identical to the flat
	// Storage CF value. Zero slots are skipped upstream, so this only ever
	// writes non-empty rows.
	if err := w.fs.putFlatStorage(addrHash, slotKeyHash, valueRLP); err != nil {
		return err
	}
	return w.b.AddStorageSlot(addrHash, slotKeyHash, valueRLP)
}

func (w *flatStateWriter) FinalizeStorageRoot(addrHash [32]byte) ([32]byte, error) {
	return w.b.FinalizeStorageRoot(addrHash)
}

// AddAccount feeds the account's full RLP to the state trie. The flat Account
// CF row (the SLIM form) is written separately by writeFlatAccountRow during
// Phase 1, where the live *types.StateAccount is available — so this hot Phase-2
// path never decodes the stashed RLP back to a StateAccount.
func (w *flatStateWriter) AddAccount(addrHash [32]byte, accountRLP []byte) error {
	return w.b.AddAccount(addrHash, accountRLP)
}

// writeFlatAccountRow tees the slim account form into the flat Account CF. It is
// called from the Phase-1 entity loops, where acc is live with its final
// storage root, so Phase 2 (AddAccount) never has to re-decode the account just
// to derive the slim leaf.
func (w *flatStateWriter) writeFlatAccountRow(addrHash [32]byte, acc *types.StateAccount) error {
	return w.fs.putFlatAccount(addrHash, nethrlp.EncodeAccountSlim(acc))
}

func (w *flatStateWriter) FinalizeStateRoot() ([32]byte, error) {
	return w.b.FinalizeStateRoot()
}

// flatSink writes state into Nethermind's flat-state RocksDB (the `flat`
// column DB). It implements nethtrie.NodeStorage — routing state/storage trie
// nodes into the four node CFs by nibble path length — and additionally exposes
// putFlatAccount / putFlatStorage for the flat leaf rows. All writes go through
// one grocksdb.WriteBatch flushed at stateBatchFlushBytes. The DB's CF handles
// are shared read-only across concurrent Phase-0 workers, each of which owns
// its own flatSink (its own WriteBatch); grocksdb.Write is safe to call
// concurrently across those per-worker batches.
type flatSink struct {
	db  *grocksdb.DB
	cfs []*grocksdb.ColumnFamilyHandle // indexed by flat.Column
	wo  *grocksdb.WriteOptions
	wb  *grocksdb.WriteBatch

	pendingBytes int
}

func newFlatSink(dbs *nethDBs) *flatSink {
	wo := grocksdb.NewDefaultWriteOptions()
	// Bulk writes skip the WAL; durability is owed at the flat-CF flush in
	// writeFlatMetadata and the final CompactRange in nethDBs.Close.
	wo.DisableWAL(true)
	return &flatSink{
		db:  dbs.flat,
		cfs: dbs.flatCFs,
		wo:  wo,
		wb:  grocksdb.NewWriteBatch(),
	}
}

func (s *flatSink) putCF(col flat.Column, key, value []byte) error {
	s.wb.PutCF(s.cfs[col], key, value)
	s.pendingBytes += len(key) + len(value)
	if s.pendingBytes >= stateBatchFlushBytes {
		return s.flush()
	}
	return nil
}

func (s *flatSink) flush() error {
	if s.pendingBytes == 0 {
		return nil
	}
	if err := s.db.Write(s.wo, s.wb); err != nil {
		return fmt.Errorf("flatSink flush: %w", err)
	}
	s.wb.Clear()
	s.pendingBytes = 0
	return nil
}

func (s *flatSink) close() error {
	err := s.flush()
	if s.wb != nil {
		s.wb.Destroy()
		s.wb = nil
	}
	if s.wo != nil {
		s.wo.Destroy()
		s.wo = nil
	}
	return err
}

// SetStateNode routes a state-trie node into its flat node CF. The keccak
// argument is unused — flat node keys are pure-path.
func (s *flatSink) SetStateNode(path []byte, pathLen int, _ [32]byte, rlpBlob []byte) error {
	col, key := flat.StateNodeKey(path, pathLen)
	return s.putCF(col, key, rlpBlob)
}

// SetStorageNode routes a storage-trie node into its flat node CF, tagged with
// the contract's address hash. The keccak argument is unused.
func (s *flatSink) SetStorageNode(addrHash [32]byte, path []byte, pathLen int, _ [32]byte, rlpBlob []byte) error {
	col, key := flat.StorageNodeKey(addrHash, path, pathLen)
	return s.putCF(col, key, rlpBlob)
}

func (s *flatSink) putFlatAccount(addrHash [32]byte, slimRLP []byte) error {
	return s.putCF(flat.ColAccount, flat.AccountKey(addrHash), slimRLP)
}

func (s *flatSink) putFlatStorage(addrHash [32]byte, slotKeyHash [32]byte, valueRLP []byte) error {
	return s.putCF(flat.ColStorage, flat.StorageKey(addrHash, slotKeyHash), valueRLP)
}

// writeFlatMetadata stamps the three Metadata-CF markers that make Nethermind
// detect and serve the DB as flat: Layout (Flat), SlotEncoding (RLP), and
// CurrentState (block 0 ‖ genesis state root). It runs once, after all flat
// state has been flushed by the sinks and before the genesis block-tree write.
//
// The flat data CFs are flushed to SST first, so the CurrentState marker — the
// boot-detection gate, written with the WAL enabled and Sync set — is only made
// durable after the state it certifies. This ordering mirrors the "blockInfos
// last" discipline of the genesis block-tree writer.
func writeFlatMetadata(dbs *nethDBs, root common.Hash) error {
	fo := grocksdb.NewDefaultFlushOptions()
	fo.SetWait(true)
	defer fo.Destroy()
	dataCFs := []*grocksdb.ColumnFamilyHandle{
		dbs.flatCFs[flat.ColAccount],
		dbs.flatCFs[flat.ColStorage],
		dbs.flatCFs[flat.ColStateNodes],
		dbs.flatCFs[flat.ColStateTopNodes],
		dbs.flatCFs[flat.ColStorageNodes],
		dbs.flatCFs[flat.ColFallbackNodes],
	}
	if err := dbs.flat.FlushCFs(dataCFs, fo); err != nil {
		return fmt.Errorf("flush flat data CFs: %w", err)
	}

	wo := grocksdb.NewDefaultWriteOptions()
	wo.SetSync(true)
	defer wo.Destroy()
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()

	meta := dbs.flatCFs[flat.ColMetadata]
	wb.PutCF(meta, flat.LayoutKey, []byte{flat.LayoutFlat})
	wb.PutCF(meta, flat.SlotEncodingKey, []byte{flat.SlotEncodingRLP})
	wb.PutCF(meta, flat.CurrentStateKey, flat.CurrentStateValue(0, root))

	if err := dbs.flat.Write(wo, wb); err != nil {
		return fmt.Errorf("write flat metadata markers: %w", err)
	}
	return nil
}
