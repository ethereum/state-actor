// Vendored from github.com/erigontech/erigon execution/commitment/commitment.go @ 14273f79a6 (production pin).
// Modifications: package commitment -> hph; build tag; nibbles import rewrite; R2+R3 strips: ReplacePlainKeys/MergeHexBranches/Validate+helpers/BranchStat/DecodeBranchAndCollectStat/ParseTrieVariant/PendingCommitmentUpdate/Touch{PlainKey,HashedKey,Account,Storage,Code}/NewEmpty/SetMode/Mode/PlainKeys/keyHasherNoop/GetDeferredUpdateMetrics (+ crypto/accounts/keccak/slices/nibbles imports); R3: Updates core (NewUpdates/TouchPlainKeyDirect/HashSort/arena/Mode/KeyUpdate/keyHasher), Trie interface + TrieVariant + InitializeTrieAndUpdates, CommitProgress, Update.Copy, BranchEncoder warmup-cache field/branches, levelled metric arrays
//
//go:build cgo_erigon_commitment

// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package hph

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/empty"
	"github.com/erigontech/erigon/common/length"
	"github.com/erigontech/erigon/common/maphash"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/diagnostics/metrics"
)

var (
	mxTrieProcessedKeys   = metrics.GetOrCreateCounter("domain_commitment_keys")
	mxTrieBranchesUpdated = metrics.GetOrCreateCounter("domain_commitment_updates_applied")

	mxTrieStateSkipRate = metrics.GetOrCreateCounter("trie_state_skip_rate")
	mxTrieStateLoadRate = metrics.GetOrCreateCounter("trie_state_load_rate")
)

type PatriciaContext interface {
	// GetBranch load branch node and fill up the cells
	// For each cell, it sets the cell type, clears the modified flag, fills the hash,
	// and for the extension, account, and leaf type, the `l` and `k`
	Branch(prefix []byte) ([]byte, kv.Step, error)
	// store branch data
	PutBranch(prefix []byte, data []byte, prevData []byte) error
	// fetch account with given plain key
	Account(plainKey []byte) (*Update, error)
	// fetch storage with given plain key
	Storage(plainKey []byte) (*Update, error)
}

type cellFields uint8

const (
	fieldExtension   cellFields = 1
	fieldAccountAddr cellFields = 2
	fieldStorageAddr cellFields = 4
	fieldHash        cellFields = 8
	fieldStateHash   cellFields = 16
)

func (p cellFields) String() string {
	var sb strings.Builder
	if p&fieldExtension != 0 {
		sb.WriteString("DownHash")
	}
	if p&fieldAccountAddr != 0 {
		sb.WriteString("+AccountPlain")
	}
	if p&fieldStorageAddr != 0 {
		sb.WriteString("+StoragePlain")
	}
	if p&fieldHash != 0 {
		sb.WriteString("+Hash")
	}
	if p&fieldStateHash != 0 {
		sb.WriteString("+LeafHash")
	}
	return sb.String()
}

// cellEncodeData contains only the fields needed for EncodeBranch.
// This is much smaller than cell (which has hashedExtension[128], Update, etc.)
// TODO: unify with cell by shrinking cell struct to eliminate this separate type
type cellEncodeData struct {
	extension   [64]byte
	accountAddr [20]byte                        // common.Address
	storageAddr [length.Addr + length.Hash]byte // 20 + 32 = 52 bytes
	hash        [32]byte                        // common.Hash
	stateHash   [32]byte                        // common.Hash

	extLen         int16
	accountAddrLen int16
	storageAddrLen int16
	hashLen        int16
	stateHashLen   int16
}

// cellEncodeDataFromCell extracts the encoding-relevant fields from a cell into a cellEncodeData.
func cellEncodeDataFromCell(c *cell) cellEncodeData {
	var d cellEncodeData
	d.extLen = c.extLen
	d.accountAddrLen = c.accountAddrLen
	d.storageAddrLen = c.storageAddrLen
	d.hashLen = c.hashLen
	d.stateHashLen = c.stateHashLen
	copy(d.extension[:], c.extension[:c.extLen])
	copy(d.accountAddr[:], c.accountAddr[:c.accountAddrLen])
	copy(d.storageAddr[:], c.storageAddr[:c.storageAddrLen])
	copy(d.hash[:], c.hash[:c.hashLen])
	copy(d.stateHash[:], c.stateHash[:c.stateHashLen])
	return d
}

// DeferredBranchUpdate holds the data needed to perform a branch update later.
// This allows collecting updates during the fold phase and running computeCellHash + EncodeBranch in parallel.
type DeferredBranchUpdate struct {
	prefix   []byte
	bitmap   uint16
	touchMap uint16
	afterMap uint16

	// Cells needed for EncodeBranch - only the fields required for encoding
	cells [16]cellEncodeData

	// Previous data from ctx.Branch (for merging)
	prev []byte
	// Result after encoding (filled by parallel workers)
	encoded BranchData
}

// Global pool for deferred branch updates.
var deferredUpdatePool = sync.Pool{
	New: func() any {
		return &DeferredBranchUpdate{}
	},
}

// Metrics for getDeferredUpdate - use atomics for thread safety
var getDeferredUpdateCount atomic.Int64

// ResetDeferredUpdateMetrics resets the getDeferredUpdate metrics.
func ResetDeferredUpdateMetrics() {
	getDeferredUpdateCount.Store(0)
}

// getDeferredUpdate gets a DeferredBranchUpdate from the global pool
// and copies only the fields needed for encoding.
func getDeferredUpdate(
	prefix []byte,
	bitmap, touchMap, afterMap uint16,
	cells *[16]cellEncodeData,
	prev []byte,
) *DeferredBranchUpdate {
	getDeferredUpdateCount.Add(1)
	upd := deferredUpdatePool.Get().(*DeferredBranchUpdate)

	upd.prefix = common.Copy(prefix)
	upd.bitmap = bitmap
	upd.touchMap = touchMap
	upd.afterMap = afterMap

	// Direct struct copy for each cell in bitmap
	for bitset := bitmap; bitset != 0; {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		upd.cells[nibble] = cells[nibble]
		bitset ^= bit
	}

	upd.prev = common.Copy(prev)
	upd.encoded = nil

	return upd
}

// putDeferredUpdate returns a DeferredBranchUpdate to the global pool.
// Clears slice references so pooled objects don't hold stale memory.
func putDeferredUpdate(upd *DeferredBranchUpdate) {
	if upd != nil {
		upd.prefix = nil
		upd.prev = nil
		upd.encoded = nil
		deferredUpdatePool.Put(upd)
	}
}

type BranchEncoder struct {
	buf       *bytes.Buffer
	bitmapBuf [binary.MaxVarintLen64]byte
	merger    *BranchMerger
	metrics   *Metrics

	// Deferred updates support
	deferUpdates    bool
	deferred        []*DeferredBranchUpdate
	pendingPrefixes *maphash.NonConcurrentMap[struct{}] // tracks pending prefixes to detect duplicates
}

func NewBranchEncoder(sz uint64) *BranchEncoder {
	return &BranchEncoder{
		buf:    bytes.NewBuffer(make([]byte, sz)),
		merger: NewHexBranchMerger(sz / 2),
	}
}

// SetDeferUpdates enables or disables deferred update collection.
// When enabled, CollectUpdate will store updates in a cache instead of applying them immediately.
func (be *BranchEncoder) SetDeferUpdates(defer_ bool) {
	be.deferUpdates = defer_
	if defer_ {
		if be.deferred == nil {
			be.deferred = make([]*DeferredBranchUpdate, 0, 64)
		}
		if be.pendingPrefixes == nil {
			be.pendingPrefixes = maphash.NewNonConcurrentMap[struct{}]()
		}
	}
}

// DeferUpdatesEnabled returns whether deferred update collection is enabled.
func (be *BranchEncoder) DeferUpdatesEnabled() bool {
	return be.deferUpdates
}

// HasPendingPrefix returns true if the given prefix has a pending deferred update.
func (be *BranchEncoder) HasPendingPrefix(prefix []byte) bool {
	if be.pendingPrefixes == nil {
		return false
	}
	_, found := be.pendingPrefixes.Get(prefix)
	return found
}

// ClearDeferred clears the deferred updates list and returns all objects to the pool.
func (be *BranchEncoder) ClearDeferred() {
	for _, upd := range be.deferred {
		putDeferredUpdate(upd)
	}
	be.deferred = be.deferred[:0]
	if be.pendingPrefixes != nil {
		be.pendingPrefixes.Clear()
	}
	ResetDeferredUpdateMetrics()
}

// encodeDeferredUpdate encodes a branch update using the provided encoder and merger.
// Cell hashes are already computed during fold() before cells were copied.
func encodeDeferredUpdate(
	upd *DeferredBranchUpdate,
	encoder *BranchEncoder,
	merger *BranchMerger,
) error {
	update, err := encoder.EncodeBranch(upd.bitmap, upd.touchMap, upd.afterMap, &upd.cells)
	if err != nil {
		return err
	}

	if len(upd.prev) > 0 {
		if bytes.Equal(upd.prev, update) {
			upd.encoded = nil // skip unchanged
			return nil
		}
		update, err = merger.Merge(upd.prev, update)
		if err != nil {
			return err
		}
	}

	upd.encoded = common.Copy(update)
	return nil
}

// ApplyDeferredUpdates encodes branch updates concurrently and writes them.
func (be *BranchEncoder) ApplyDeferredUpdates(
	numWorkers int,
	putBranch func(prefix []byte, data []byte, prevData []byte) error,
) error {
	written, err := ApplyDeferredBranchUpdates(be.deferred, numWorkers, putBranch)
	if err != nil {
		return err
	}
	if be.metrics != nil {
		be.metrics.updateBranch.Add(uint64(written))
	}
	return nil
}

// Pools for worker encoders/mergers to avoid per-call allocations.
var (
	workerEncoderPool = sync.Pool{New: func() any { return NewBranchEncoder(1024) }}
	workerMergerPool  = sync.Pool{New: func() any { return NewHexBranchMerger(512) }}
)

// ApplyDeferredBranchUpdates encodes deferred branch updates concurrently and writes them.
// Returns the number of updates successfully written.
func ApplyDeferredBranchUpdates(
	deferred []*DeferredBranchUpdate,
	numWorkers int,
	putBranch func(prefix []byte, data []byte, prevData []byte) error,
) (int, error) {
	if len(deferred) == 0 {
		return 0, nil
	}
	if numWorkers <= 1 {
		numWorkers = 1
	}

	// Sequential fast path: avoids goroutine and channel overhead for small batches.
	if numWorkers == 1 || len(deferred) <= numWorkers {
		encoder := workerEncoderPool.Get().(*BranchEncoder)
		merger := workerMergerPool.Get().(*BranchMerger)
		defer workerEncoderPool.Put(encoder)
		defer workerMergerPool.Put(merger)

		var written int
		for _, upd := range deferred {
			if err := encodeDeferredUpdate(upd, encoder, merger); err != nil {
				return written, err
			}
			if upd.encoded == nil {
				continue
			}
			if err := putBranch(upd.prefix, upd.encoded, upd.prev); err != nil {
				return written, err
			}
			written++
		}
		mxTrieBranchesUpdated.AddInt(written)
		return written, nil
	}

	// Pipeline: workers encode in parallel, results sent to channel, main goroutine writes sequentially.
	type result struct {
		upd *DeferredBranchUpdate
		err error
	}
	// Size channels to actual batch length, not the 50K max.
	resultCh := make(chan result, len(deferred))
	workCh := make(chan *DeferredBranchUpdate, len(deferred))

	// Start workers with pooled encoders/mergers.
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			encoder := workerEncoderPool.Get().(*BranchEncoder)
			merger := workerMergerPool.Get().(*BranchMerger)
			defer workerEncoderPool.Put(encoder)
			defer workerMergerPool.Put(merger)

			for upd := range workCh {
				err := encodeDeferredUpdate(upd, encoder, merger)
				resultCh <- result{upd: upd, err: err}
			}
		}()
	}

	// Close resultCh when all workers are done
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Send work in background
	go func() {
		for _, upd := range deferred {
			workCh <- upd
		}
		close(workCh)
	}()

	// Process results as they come in - write to storage immediately
	var firstErr error
	var written int
	for res := range resultCh {
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		if res.upd.encoded == nil {
			continue // skip unchanged
		}
		if firstErr != nil {
			continue // drain channel but don't write after error
		}
		if err := putBranch(res.upd.prefix, res.upd.encoded, res.upd.prev); err != nil {
			firstErr = err
			continue
		}
		written++
	}
	mxTrieBranchesUpdated.AddInt(written)
	return written, firstErr
}

func (be *BranchEncoder) setMetrics(metrics *Metrics) {
	be.metrics = metrics
}

func (be *BranchEncoder) CollectUpdate(
	ctx PatriciaContext,
	prefix []byte,
	bitmap, touchMap, afterMap uint16,
	cells *[16]cellEncodeData,
) error {
	prev, _, err := ctx.Branch(prefix)
	if err != nil {
		return err
	}

	update, err := be.EncodeBranch(bitmap, touchMap, afterMap, cells)
	if err != nil {
		return err
	}

	if len(prev) > 0 {
		if bytes.Equal(prev, update) {
			return nil // do not write the same data for prefix
		}
		update, err = be.merger.Merge(prev, update)
		if err != nil {
			return err
		}
	}

	prefixCopy := common.Copy(prefix)
	updateCopy := common.Copy(update)
	if err = ctx.PutBranch(prefixCopy, updateCopy, prev); err != nil {
		return err
	}
	if be.metrics != nil {
		be.metrics.updateBranch.Add(1)
	}
	mxTrieBranchesUpdated.Inc()
	return nil
}

const maxDeferredUpdates = 50_000

// CollectDeferredUpdate stores a branch update job for later parallel processing.
// Unlike CollectUpdate, this does NOT call EncodeBranch immediately - it copies the cellEncodeData
// and defers encoding for parallel execution later.
// Cell hashes are already computed during fold() before this is called.
// Flushes pending updates if a duplicate prefix is detected or if deferred count exceeds maxDeferredUpdates.
func (be *BranchEncoder) CollectDeferredUpdate(
	ctx PatriciaContext,
	prefix []byte,
	bitmap, touchMap, afterMap uint16,
	cells *[16]cellEncodeData,
) error {
	// Flush if duplicate prefix or too many deferred updates
	needsFlush := len(be.deferred) >= maxDeferredUpdates
	if !needsFlush {
		_, needsFlush = be.pendingPrefixes.Get(prefix)
	}

	if needsFlush {
		if err := be.ApplyDeferredUpdates(16, ctx.PutBranch); err != nil {
			return err
		}
		be.ClearDeferred()
	}

	prev, _, err := ctx.Branch(prefix)
	if err != nil {
		return err
	}

	// Track this prefix as pending
	be.pendingPrefixes.Set(prefix, struct{}{})

	// Get a pooled DeferredBranchUpdate and copy all fields
	upd := getDeferredUpdate(prefix, bitmap, touchMap, afterMap, cells, prev)
	be.deferred = append(be.deferred, upd)
	return nil
}

func (be *BranchEncoder) putUvarAndVal(size uint64, val []byte) error {
	n := binary.PutUvarint(be.bitmapBuf[:], size)
	if _, err := be.buf.Write(be.bitmapBuf[:n]); err != nil {
		return err
	}
	if _, err := be.buf.Write(val); err != nil {
		return err
	}
	return nil
}

// EncodeBranch encodes branch data from cellEncodeData. Pure serializer with no side effects.
// Result should be copied before next call to EncodeBranch, underlying slice is reused.
func (be *BranchEncoder) EncodeBranch(bitmap, touchMap, afterMap uint16, cells *[16]cellEncodeData) (BranchData, error) {
	be.buf.Reset()

	var encoded [4]byte
	binary.BigEndian.PutUint16(encoded[:], touchMap)
	binary.BigEndian.PutUint16(encoded[2:], afterMap)
	if _, err := be.buf.Write(encoded[:]); err != nil {
		return nil, err
	}

	for bitset := afterMap; bitset != 0; {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		cell := &cells[nibble]

		if bitmap&bit != 0 {
			var fields cellFields
			if cell.extLen > 0 && cell.storageAddrLen == 0 {
				fields |= fieldExtension
			}
			if cell.accountAddrLen > 0 {
				fields |= fieldAccountAddr
			}
			if cell.storageAddrLen > 0 {
				fields |= fieldStorageAddr
			}
			if cell.hashLen > 0 {
				fields |= fieldHash
			}
			if cell.stateHashLen == 32 && (cell.accountAddrLen > 0 || cell.storageAddrLen > 0) {
				fields |= fieldStateHash
			}
			if err := be.buf.WriteByte(byte(fields)); err != nil {
				return nil, err
			}
			if fields&fieldExtension != 0 {
				if err := be.putUvarAndVal(uint64(cell.extLen), cell.extension[:cell.extLen]); err != nil {
					return nil, err
				}
			}
			if fields&fieldAccountAddr != 0 {
				if err := be.putUvarAndVal(uint64(cell.accountAddrLen), cell.accountAddr[:cell.accountAddrLen]); err != nil {
					return nil, err
				}
			}
			if fields&fieldStorageAddr != 0 {
				if err := be.putUvarAndVal(uint64(cell.storageAddrLen), cell.storageAddr[:cell.storageAddrLen]); err != nil {
					return nil, err
				}
			}
			if fields&fieldHash != 0 {
				if err := be.putUvarAndVal(uint64(cell.hashLen), cell.hash[:cell.hashLen]); err != nil {
					return nil, err
				}
			}
			if fields&fieldStateHash != 0 {
				if err := be.putUvarAndVal(uint64(cell.stateHashLen), cell.stateHash[:cell.stateHashLen]); err != nil {
					return nil, err
				}
			}
		}
		bitset ^= bit
	}
	return be.buf.Bytes(), nil
}

type BranchData []byte

func (branchData BranchData) String() string {
	if len(branchData) == 0 {
		return ""
	}
	touchMap := binary.BigEndian.Uint16(branchData[0:])
	afterMap := binary.BigEndian.Uint16(branchData[2:])
	pos := 4
	var sb strings.Builder
	var cell cell
	fmt.Fprintf(&sb, "(%d) touchMap %016b, afterMap %016b\n", len(branchData), touchMap, afterMap)
	for bitset, j := touchMap, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		fmt.Fprintf(&sb, "   %x => ", nibble)
		if afterMap&bit == 0 {
			sb.WriteString("{DELETED}\n")
		} else {
			fields := cellFields(branchData[pos])
			pos++
			var err error
			if pos, err = cell.fillFromFields(branchData, pos, fields); err != nil {
				// This is used for test output, so ok to panic
				panic(err)
			}
			sb.WriteString("{")
			var comma string
			if cell.hashedExtLen > 0 {
				fmt.Fprintf(&sb, "hashedExtension=[%x]", cell.hashedExtension[:cell.hashedExtLen])
				comma = ","
			}
			if cell.accountAddrLen > 0 {
				fmt.Fprintf(&sb, "%saccountAddr=[%x]", comma, cell.accountAddr[:cell.accountAddrLen])
				comma = ","
			}
			if cell.storageAddrLen > 0 {
				fmt.Fprintf(&sb, "%sstorageAddr=[%x]", comma, cell.storageAddr[:cell.storageAddrLen])
				comma = ","
			}
			if cell.hashLen > 0 {
				fmt.Fprintf(&sb, "%shash=[%x]", comma, cell.hash[:cell.hashLen])
			}
			if cell.stateHashLen > 0 {
				fmt.Fprintf(&sb, "%sleafHash=[%x]", comma, cell.stateHash[:cell.stateHashLen])
			}
			sb.WriteString("}\n")
		}
		bitset ^= bit
	}
	return sb.String()
}

type BranchMerger struct {
	buf []byte
	num [4]byte
}

func NewHexBranchMerger(capacity uint64) *BranchMerger {
	return &BranchMerger{buf: make([]byte, capacity)}
}

// MergeHexBranches combines two branchData, number 2 coming after (and potentially shadowing) number 1
func (m *BranchMerger) Merge(branch1 BranchData, branch2 BranchData) (BranchData, error) {
	if len(branch2) == 0 {
		return branch1, nil
	}
	if len(branch1) == 0 {
		return branch2, nil
	}

	touchMap1 := binary.BigEndian.Uint16(branch1[0:])
	afterMap1 := binary.BigEndian.Uint16(branch1[2:])
	bitmap1 := touchMap1 & afterMap1
	pos1 := 4

	touchMap2 := binary.BigEndian.Uint16(branch2[0:])
	afterMap2 := binary.BigEndian.Uint16(branch2[2:])
	bitmap2 := touchMap2 & afterMap2
	pos2 := 4

	binary.BigEndian.PutUint16(m.num[0:], touchMap1|touchMap2)
	binary.BigEndian.PutUint16(m.num[2:], afterMap2)
	dataPos := 4

	m.buf = append(m.buf[:0], m.num[:]...)

	for bitset, j := bitmap1|bitmap2, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		if bitmap2&bit != 0 {
			// Add fields from branch2
			fields := cellFields(branch2[pos2])
			m.buf = append(m.buf, byte(fields))
			pos2++

			for i := 0; i < bits.OnesCount8(byte(fields)); i++ {
				l, n := binary.Uvarint(branch2[pos2:])
				if n == 0 {
					return nil, errors.New("MergeHexBranches branch2 is too small: expected node info size")
				} else if n < 0 {
					return nil, errors.New("MergeHexBranches branch2: size overflow for length")
				}

				m.buf = append(m.buf, branch2[pos2:pos2+n]...)
				pos2 += n
				dataPos += n
				if len(branch2) < pos2+int(l) {
					return nil, fmt.Errorf("MergeHexBranches branch2 is too small: expected at least %d got %d bytes", pos2+int(l), len(branch2))
				}
				if l > 0 {
					m.buf = append(m.buf, branch2[pos2:pos2+int(l)]...)
					pos2 += int(l)
					dataPos += int(l)
				}
			}
		}
		if bitmap1&bit != 0 {
			add := (touchMap2&bit == 0) && (afterMap2&bit != 0) // Add fields from branchData1
			fields := cellFields(branch1[pos1])
			if add {
				m.buf = append(m.buf, byte(fields))
			}
			pos1++
			for i := 0; i < bits.OnesCount8(byte(fields)); i++ {
				l, n := binary.Uvarint(branch1[pos1:])
				if n == 0 {
					return nil, errors.New("MergeHexBranches branch1 is too small: expected node info size")
				} else if n < 0 {
					return nil, errors.New("MergeHexBranches branch1: size overflow for length")
				}

				if add {
					m.buf = append(m.buf, branch1[pos1:pos1+n]...)
				}
				pos1 += n
				if len(branch1) < pos1+int(l) {
					return nil, fmt.Errorf("MergeHexBranches branch1 is too small: expected at least %d got %d bytes", pos1+int(l), len(branch1))
				}
				if l > 0 {
					if add {
						m.buf = append(m.buf, branch1[pos1:pos1+int(l)]...)
					}
					pos1 += int(l)
				}
			}
		}
		bitset ^= bit
	}
	return m.buf, nil
}

type UpdateFlags uint8

const (
	CodeUpdate    UpdateFlags = 1
	DeleteUpdate  UpdateFlags = 2
	BalanceUpdate UpdateFlags = 4
	NonceUpdate   UpdateFlags = 8
	StorageUpdate UpdateFlags = 16
)

func (uf UpdateFlags) String() string {
	var sb strings.Builder
	if uf&DeleteUpdate != 0 {
		sb.WriteString("Delete")
	}
	if uf&BalanceUpdate != 0 {
		sb.WriteString("+Balance")
	}
	if uf&NonceUpdate != 0 {
		sb.WriteString("+Nonce")
	}
	if uf&CodeUpdate != 0 {
		sb.WriteString("+Code")
	}
	if uf&StorageUpdate != 0 {
		sb.WriteString("+Storage")
	}
	return sb.String()
}

type Update struct {
	CodeHash   common.Hash
	Storage    common.Hash
	StorageLen int8
	Flags      UpdateFlags
	Balance    uint256.Int
	Nonce      uint64
}

func (u *Update) Reset() {
	u.Flags = 0
	u.Balance.Clear()
	u.Nonce = 0
	u.StorageLen = 0
	u.CodeHash = empty.CodeHash
}

func (u *Update) Merge(b *Update) {
	if b.Flags == DeleteUpdate {
		u.Flags = DeleteUpdate
		return
	}
	if b.Flags&(BalanceUpdate|NonceUpdate|CodeUpdate|StorageUpdate) != 0 {
		u.Flags &^= DeleteUpdate
	}
	if b.Flags&BalanceUpdate != 0 {
		u.Flags |= BalanceUpdate
		u.Balance.Set(&b.Balance)
	}
	if b.Flags&NonceUpdate != 0 {
		u.Flags |= NonceUpdate
		u.Nonce = b.Nonce
	}
	if b.Flags&CodeUpdate != 0 {
		u.Flags |= CodeUpdate
		copy(u.CodeHash[:], b.CodeHash[:])
	}
	if b.Flags&StorageUpdate != 0 {
		u.Flags |= StorageUpdate
		copy(u.Storage[:], b.Storage[:b.StorageLen])
		u.StorageLen = b.StorageLen
	}
}

func (u *Update) Encode(buf []byte, numBuf []byte) []byte {
	buf = append(buf, byte(u.Flags))
	if u.Flags&BalanceUpdate != 0 {
		buf = append(buf, byte(u.Balance.ByteLen()))
		buf = append(buf, u.Balance.Bytes()...)
	}
	if u.Flags&NonceUpdate != 0 {
		n := binary.PutUvarint(numBuf, u.Nonce)
		buf = append(buf, numBuf[:n]...)
	}
	if u.Flags&CodeUpdate != 0 {
		buf = append(buf, u.CodeHash[:]...)
	}
	if u.Flags&StorageUpdate != 0 {
		n := binary.PutUvarint(numBuf, uint64(u.StorageLen))
		buf = append(buf, numBuf[:n]...)
		if u.StorageLen > 0 {
			buf = append(buf, u.Storage[:u.StorageLen]...)
		}
	}
	return buf
}

func (u *Update) Deleted() bool {
	return u.Flags&DeleteUpdate > 0
}

func (u *Update) Decode(buf []byte, pos int) (int, error) {
	if len(buf) < pos+1 {
		return 0, errors.New("decode Update: buffer too small for flags")
	}
	u.Reset()

	u.Flags = UpdateFlags(buf[pos])
	pos++
	if u.Flags&BalanceUpdate != 0 {
		if len(buf) < pos+1 {
			return 0, errors.New("decode Update: buffer too small for balance len")
		}
		balanceLen := int(buf[pos])
		pos++
		if len(buf) < pos+balanceLen {
			return 0, errors.New("decode Update: buffer too small for balance")
		}
		u.Balance.SetBytes(buf[pos : pos+balanceLen])
		pos += balanceLen
	}
	if u.Flags&NonceUpdate != 0 {
		var n int
		u.Nonce, n = binary.Uvarint(buf[pos:])
		if n == 0 {
			return 0, errors.New("decode Update: buffer too small for nonce")
		}
		if n < 0 {
			return 0, errors.New("decode Update: nonce overflow")
		}
		pos += n
	}
	if u.Flags&CodeUpdate != 0 {
		if len(buf) < pos+length.Hash {
			return 0, errors.New("decode Update: buffer too small for codeHash")
		}
		copy(u.CodeHash[:], buf[pos:pos+32])
		pos += length.Hash
	}
	if u.Flags&StorageUpdate != 0 {
		l, n := binary.Uvarint(buf[pos:])
		if n == 0 {
			return 0, errors.New("decode Update: buffer too small for storage len")
		}
		if n < 0 {
			return 0, errors.New("decode Update: storage pos overflow")
		}
		pos += n
		if len(buf) < pos+int(l) {
			return 0, errors.New("decode Update: buffer too small for storage")
		}
		u.StorageLen = int8(l)
		copy(u.Storage[:], buf[pos:pos+int(u.StorageLen)])
		pos += int(u.StorageLen)
	}
	return pos, nil
}

func (u *Update) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Flags: [%s]", u.Flags))
	if u.Deleted() {
		sb.WriteString(", DELETED")
	}
	if u.Flags&BalanceUpdate != 0 {
		sb.WriteString(fmt.Sprintf(", Balance: [%d]", &u.Balance))
	}
	if u.Flags&NonceUpdate != 0 {
		sb.WriteString(fmt.Sprintf(", Nonce: [%d]", u.Nonce))
	}
	if u.Flags&CodeUpdate != 0 {
		sb.WriteString(fmt.Sprintf(", CodeHash: [%x]", u.CodeHash))
	}
	if u.Flags&StorageUpdate != 0 {
		sb.WriteString(fmt.Sprintf(", Storage: [%x]", u.Storage[:u.StorageLen]))
	}
	return sb.String()
}
