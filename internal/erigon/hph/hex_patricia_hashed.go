// Vendored from github.com/erigontech/erigon execution/commitment/hex_patricia_hashed.go @ 14273f79a6 (production pin).
// Modifications: package commitment -> hph; build tag; nibbles import rewrite; stripped 5 funcs (GenerateWitness, toWitnessTrie, witnessCreateAccountNode, witnessComputeCellHashWithStorage, PrintGrid — debug-only) + 4 imports (commitment/trie, witness, common/crypto, types/accounts); R2 dead-code strip: SetState/state.Decode/cell.Decode/HexTrie* readers, feedBranchHashesToKeccak, Grid, DomainPutter/CommitmentWrite (+stateifs), collapse-tracer cluster
//
//go:build cgo_erigon_commitment

// Copyright 2022 The Erigon Authors
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
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"runtime"
	"strings"
	"sync/atomic"

	keccak "github.com/erigontech/fastkeccak"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/dbg"
	"github.com/erigontech/erigon/common/empty"
	"github.com/erigontech/erigon/common/length"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/ethereum/state-actor/internal/erigon/hph/nibbles"
)

// HexPatriciaHashed implements commitment based on patricia merkle tree with radix 16,
// with keys pre-hashed by keccak256
type HexPatriciaHashed struct {
	root cell // Root cell of the tree
	// How many rows (starting from row 0) are currently active and have corresponding selected columns
	// Last active row does not have selected column
	activeRows int
	// Length of the key that reflects current positioning of the grid. It may be larger than number of active rows,
	// if an account leaf cell represents multiple nibbles in the key
	currentKeyLen int16
	accountKeyLen int16
	// Rows of the grid correspond to the level of depth in the patricia tree
	// Columns of the grid correspond to pointers to the nodes further from the root
	grid          [128][16]cell // First 64 rows of this grid are for account trie, and next 64 rows are for storage trie
	currentKey    [128]byte     // For each row indicates which column is currently selected
	depths        [128]int16    // For each row, the depth of cells in that row
	branchBefore  [128]bool     // For each row, whether there was a branch node in the database loaded in unfold
	touchMap      [128]uint16   // For each row, bitmap of cells that were either present before modification, or modified or deleted
	afterMap      [128]uint16   // For each row, bitmap of cells that were present after modification
	keccak        keccak.KeccakState
	keccak2       keccak.KeccakState
	rootChecked   bool // Set to false if it is not known whether the root is empty, set to true if it is checked
	rootTouched   bool
	rootPresent   bool
	trace         bool
	ctx           PatriciaContext
	hashAuxBuffer [128]byte     // buffer to compute cell hash or write hash-related things
	cellHashBuf   common.Hash   // shared scratch buffer for hashKey calls (avoids per-cell allocation)
	auxBuffer     *bytes.Buffer // auxiliary buffer used during branch updates encoding
	branchEncoder *BranchEncoder

	mounted    bool // true if this trie is mounted to some root trie
	mountedNib int  // if 0 <= nib <= 15 means mounted to some root. If -1, means it's a storage subtrie so must not be folded above depth 63

	memoizationOff bool // if true, do not rely on memoized hashes
	//temp buffers
	accValBuf rlp.RlpEncodedBytes

	//processing metrics
	metrics       *Metrics
	depthsToTxNum [129]uint64 // endTxNum of file with branch data for that depth
	hadToLoadL    map[uint64]skipStat
}

// Clones current trie state to allow concurrent processing.
func (hph *HexPatriciaHashed) SpawnSubTrie(ctx PatriciaContext, forNibble int) *HexPatriciaHashed {
	subTrie := NewHexPatriciaHashed(hph.accountKeyLen, ctx)
	// Disable deferred updates for sub-tries since they fold directly
	// and their deferred updates would never be applied
	subTrie.branchEncoder.SetDeferUpdates(false)

	subTrie.mountTo(hph, forNibble)
	return subTrie
}

func NewHexPatriciaHashed(accountKeyLen int16, ctx PatriciaContext) *HexPatriciaHashed {
	hph := newHexPatriciaHashed()
	hph.accountKeyLen = accountKeyLen
	hph.ctx = ctx
	return hph
}

func newHexPatriciaHashed() *HexPatriciaHashed {
	hph := &HexPatriciaHashed{
		keccak:        keccak.NewFastKeccak(),
		keccak2:       keccak.NewFastKeccak(),
		auxBuffer:     bytes.NewBuffer(make([]byte, 8192)),
		hadToLoadL:    make(map[uint64]skipStat),
		accValBuf:     make(rlp.RlpEncodedBytes, 128),
		metrics:       NewMetrics(),
		branchEncoder: NewBranchEncoder(1024),
	}

	hph.branchEncoder.setMetrics(hph.metrics)
	hph.branchEncoder.SetDeferUpdates(true) // Enable deferred branch updates by default
	return hph
}

type cell struct {
	hashedExtension [128]byte
	extension       [64]byte
	accountAddr     common.Address                  // account plain key
	storageAddr     [length.Addr + length.Hash]byte // storage plain key
	hash            common.Hash                     // cell hash
	stateHash       common.Hash
	hashedExtLen    int16     // length of the hashed extension, if any
	extLen          int16     // length of the extension, if any
	accountAddrLen  int16     // length of account plain key
	storageAddrLen  int16     // length of the storage plain key
	hashLen         int16     // Length of the hash (or embedded)
	stateHashLen    int16     // stateHash length, if > 0 can reuse
	loaded          loadFlags // folded Cell have only hash, unfolded have all fields
	Update                    // state update
}

type loadFlags uint8

const (
	cellLoadNone    = loadFlags(0)
	cellLoadAccount = loadFlags(1)
	cellLoadStorage = loadFlags(2)
)

func (f loadFlags) String() string {
	var b strings.Builder
	if f == cellLoadNone {
		b.WriteString("false")
	} else {
		if f.account() {
			b.WriteString("Account ")
		}
		if f.storage() {
			b.WriteString("Storage ")
		}
	}
	return b.String()
}

func (f loadFlags) account() bool {
	return f&cellLoadAccount != 0
}

func (f loadFlags) storage() bool {
	return f&cellLoadStorage != 0
}

func (f loadFlags) addFlag(loadFlags loadFlags) loadFlags {
	if loadFlags == cellLoadNone {
		return f
	}
	return f | loadFlags
}

var (
	emptyRootHashBytes = empty.RootHash.Bytes()
)

func (cell *cell) hashAccKey(keccak keccak.KeccakState, depth int16, hashBuf []byte) error {
	return hashKey(keccak, cell.accountAddr[:cell.accountAddrLen], cell.hashedExtension[:], depth, hashBuf)
}

func (cell *cell) hashStorageKey(keccak keccak.KeccakState, accountKeyLen, downOffset int16, hashedKeyOffset int16, hashBuf []byte) error {
	return hashKey(keccak, cell.storageAddr[accountKeyLen:cell.storageAddrLen], cell.hashedExtension[downOffset:], hashedKeyOffset, hashBuf)
}

func (cell *cell) reset() {
	cell.accountAddrLen = 0
	cell.storageAddrLen = 0
	cell.hashedExtLen = 0
	cell.extLen = 0
	cell.hashLen = 0
	cell.stateHashLen = 0
	cell.loaded = cellLoadNone
	clear(cell.hashedExtension[:])
	clear(cell.extension[:])
	clear(cell.accountAddr[:])
	clear(cell.storageAddr[:])
	clear(cell.hash[:])
	cell.Update.Reset()
}

func (cell *cell) FullString() string {
	b := new(strings.Builder)
	b.WriteString("{")
	b.WriteString(fmt.Sprintf("loaded=%v", cell.loaded))
	if cell.Deleted() {
		b.WriteString(" DELETED ")
	}

	if cell.accountAddrLen > 0 {
		b.WriteString(fmt.Sprintf(" addr=%x", cell.accountAddr[:cell.accountAddrLen]))
		b.WriteString(fmt.Sprintf(" balance=%s", cell.Balance.String()))
		b.WriteString(fmt.Sprintf(" nonce=%d", cell.Nonce))
		if cell.CodeHash != empty.CodeHash {
			b.WriteString(fmt.Sprintf(" codeHash=%x", cell.CodeHash[:]))
		} else {
			b.WriteString(" codeHash=EMPTY")
		}
	}
	if cell.storageAddrLen > 0 {
		b.WriteString(fmt.Sprintf(" addr[s]=%x", cell.storageAddr[:cell.storageAddrLen]))
		b.WriteString(fmt.Sprintf(" storage=%x", cell.Storage[:cell.StorageLen]))
	}
	if cell.hashLen > 0 {
		b.WriteString(fmt.Sprintf(" h=%x", cell.hash[:cell.hashLen]))
	}
	if cell.stateHashLen > 0 {
		b.WriteString(fmt.Sprintf(" memHash=%x", cell.stateHash[:cell.stateHashLen]))
	}
	if cell.extLen > 0 {
		b.WriteString(fmt.Sprintf(" extension=%x", cell.extension[:cell.extLen]))
	}
	if cell.hashedExtLen > 0 {
		b.WriteString(fmt.Sprintf(" hashedExtension=%x", cell.hashedExtension[:cell.hashedExtLen]))
	}

	b.WriteString("}")
	return b.String()
}

func (cell *cell) setFromUpdate(update *Update) {
	cell.Update.Merge(update)
	if update.Flags&StorageUpdate != 0 {
		cell.loaded = cell.loaded.addFlag(cellLoadStorage)
		mxTrieStateLoadRate.Inc()
		hadToLoad.Add(1)
	}
	if update.Flags&BalanceUpdate != 0 || update.Flags&NonceUpdate != 0 || update.Flags&CodeUpdate != 0 {
		cell.loaded = cell.loaded.addFlag(cellLoadAccount)
		mxTrieStateLoadRate.Inc()
		hadToLoad.Add(1)
	}
}

func (cell *cell) fillFromUpperCell(upCell *cell, depth, depthIncrement int16) {
	if upCell.hashedExtLen >= depthIncrement {
		cell.hashedExtLen = upCell.hashedExtLen - depthIncrement
	} else {
		cell.hashedExtLen = 0
	}
	if upCell.hashedExtLen > depthIncrement {
		copy(cell.hashedExtension[:], upCell.hashedExtension[depthIncrement:upCell.hashedExtLen])
	}
	if upCell.extLen >= depthIncrement {
		cell.extLen = upCell.extLen - depthIncrement
	} else {
		cell.extLen = 0
	}
	if upCell.extLen > depthIncrement {
		copy(cell.extension[:], upCell.extension[depthIncrement:upCell.extLen])
	}
	if depth <= 64 {
		cell.accountAddrLen = upCell.accountAddrLen
		if upCell.accountAddrLen > 0 {
			copy(cell.accountAddr[:], upCell.accountAddr[:cell.accountAddrLen])
			cell.Balance.Set(&upCell.Balance)
			cell.Nonce = upCell.Nonce
			cell.CodeHash = upCell.CodeHash
			cell.extLen = upCell.extLen
			if upCell.extLen > 0 {
				copy(cell.extension[:], upCell.extension[:upCell.extLen])
			}
		}
	} else {
		cell.accountAddrLen = 0
	}
	cell.storageAddrLen = upCell.storageAddrLen
	if upCell.storageAddrLen > 0 {
		copy(cell.storageAddr[:], upCell.storageAddr[:upCell.storageAddrLen])
		cell.StorageLen = upCell.StorageLen
		if upCell.StorageLen > 0 {
			copy(cell.Storage[:], upCell.Storage[:upCell.StorageLen])
		}
	}
	cell.hashLen = upCell.hashLen
	if upCell.hashLen > 0 {
		copy(cell.hash[:], upCell.hash[:upCell.hashLen])
	}
	cell.loaded = upCell.loaded
}

// fillFromLowerCell fills the cell with the data from the cell of the lower row during fold
func (cell *cell) fillFromLowerCell(lowCell *cell, lowDepth int16, preExtension []byte, nibble int) {
	if lowCell.accountAddrLen > 0 || lowDepth < 64 {
		cell.accountAddrLen = lowCell.accountAddrLen
	}
	if lowCell.accountAddrLen > 0 {
		copy(cell.accountAddr[:], lowCell.accountAddr[:cell.accountAddrLen])
		cell.Balance.Set(&lowCell.Balance)
		cell.Nonce = lowCell.Nonce
		cell.CodeHash = lowCell.CodeHash
	}
	cell.storageAddrLen = lowCell.storageAddrLen
	if lowCell.storageAddrLen > 0 {
		copy(cell.storageAddr[:], lowCell.storageAddr[:cell.storageAddrLen])
		cell.StorageLen = lowCell.StorageLen
		if lowCell.StorageLen > 0 {
			copy(cell.Storage[:], lowCell.Storage[:lowCell.StorageLen])
		}
	}
	if lowCell.hashLen > 0 {
		if (lowCell.accountAddrLen == 0 && lowDepth < 64) || (lowCell.storageAddrLen == 0 && lowDepth > 64) {
			// Extension is related to either accounts branch node, or storage branch node, we prepend it by preExtension | nibble
			if len(preExtension) > 0 {
				copy(cell.extension[:], preExtension)
			}
			cell.extension[len(preExtension)] = byte(nibble)
			if lowCell.extLen > 0 {
				copy(cell.extension[1+len(preExtension):], lowCell.extension[:lowCell.extLen])
			}
			cell.extLen = lowCell.extLen + 1 + int16(len(preExtension))
		} else {
			// Extension is related to a storage branch node, so we copy it upwards as is
			cell.extLen = lowCell.extLen
			if lowCell.extLen > 0 {
				copy(cell.extension[:], lowCell.extension[:lowCell.extLen])
			}
		}
	}
	cell.hashLen = lowCell.hashLen
	if lowCell.hashLen > 0 {
		copy(cell.hash[:], lowCell.hash[:lowCell.hashLen])
	}
	cell.loaded = lowCell.loaded
}

func (cell *cell) deriveHashedKeys(depth int16, keccak keccak.KeccakState, accountKeyLen int16, hashBuf []byte) error {
	extraLen := int16(0)
	if cell.accountAddrLen > 0 {
		if depth > 64 {
			return errors.New("deriveHashedKeys accountAddr present at depth > 64")
		}
		extraLen = 64 - depth
	}
	if cell.storageAddrLen > 0 {
		if depth >= 64 {
			extraLen = 128 - depth
		} else {
			extraLen += 64
		}
	}
	if extraLen > 0 {
		if cell.hashedExtLen > 0 {
			copy(cell.hashedExtension[extraLen:], cell.hashedExtension[:cell.hashedExtLen])
		}
		cell.hashedExtLen = min(extraLen+cell.hashedExtLen, int16(len(cell.hashedExtension)))
		var hashedKeyOffset, downOffset int16
		if cell.accountAddrLen > 0 {
			if err := cell.hashAccKey(keccak, depth, hashBuf); err != nil {
				return err
			}
			downOffset = 64 - depth
		}
		if cell.storageAddrLen > 0 {
			if depth >= 64 {
				hashedKeyOffset = depth - 64
			}
			if depth == 0 {
				accountKeyLen = 0
			}
			if err := cell.hashStorageKey(keccak, accountKeyLen, downOffset, hashedKeyOffset, hashBuf); err != nil {
				return err
			}
		}
	}
	return nil
}

func (cell *cell) fillFromFields(data []byte, pos int, fieldBits cellFields) (int, error) {
	fields := []struct {
		flag      cellFields
		lenField  *int16
		dataField []byte
		extraFunc func(int16)
	}{
		{fieldExtension, &cell.hashedExtLen, cell.hashedExtension[:], func(l int16) {
			cell.extLen = l
			if l > 0 {
				copy(cell.extension[:], cell.hashedExtension[:l])
			}
		}},
		{fieldAccountAddr, &cell.accountAddrLen, cell.accountAddr[:], nil},
		{fieldStorageAddr, &cell.storageAddrLen, cell.storageAddr[:], nil},
		{fieldHash, &cell.hashLen, cell.hash[:], nil},
		{fieldStateHash, &cell.stateHashLen, cell.stateHash[:], nil},
	}

	for _, f := range fields {
		if fieldBits&f.flag != 0 {
			l, n, err := readUvarint(data[pos:])
			if err != nil {
				return 0, err
			}
			pos += n

			if len(data) < pos+int(l) {
				return 0, fmt.Errorf("buffer too small for %v", f.flag)
			}

			*f.lenField = int16(l)
			if l > 0 {
				copy(f.dataField, data[pos:pos+int(l)])
				pos += int(l)
			}
			if f.extraFunc != nil {
				f.extraFunc(int16(l))
			}
		} else {
			*f.lenField = 0
			if f.flag == fieldExtension {
				cell.extLen = 0
			}
		}
	}

	if fieldBits&fieldAccountAddr != 0 {
		cell.CodeHash = empty.CodeHash
	}
	return pos, nil
}

func readUvarint(data []byte) (uint64, int, error) {
	l, n := binary.Uvarint(data)
	if n == 0 {
		return 0, 0, errors.New("buffer too small for length")
	} else if n < 0 {
		return 0, 0, errors.New("value overflow for length")
	}
	return l, n, nil
}

func (cell *cell) accountForHashing(buffer []byte, storageRootHash common.Hash) int {
	balanceBytes := 0
	if !cell.Balance.LtUint64(128) {
		balanceBytes = cell.Balance.ByteLen()
	}

	var nonceBytes int
	if cell.Nonce < 128 && cell.Nonce != 0 {
		nonceBytes = 0
	} else {
		nonceBytes = common.BitLenToByteLen(bits.Len64(cell.Nonce))
	}

	var structLength = uint(balanceBytes + nonceBytes + 2)
	structLength += 66 // Two 32-byte arrays + 2 prefixes

	var pos int
	if structLength < 56 {
		buffer[0] = byte(192 + structLength)
		pos = 1
	} else {
		lengthBytes := common.BitLenToByteLen(bits.Len(structLength))
		buffer[0] = byte(247 + lengthBytes)

		for i := lengthBytes; i > 0; i-- {
			buffer[i] = byte(structLength)
			structLength >>= 8
		}

		pos = lengthBytes + 1
	}

	// Encoding nonce
	if cell.Nonce < 128 && cell.Nonce != 0 {
		buffer[pos] = byte(cell.Nonce)
	} else {
		buffer[pos] = byte(128 + nonceBytes)
		var nonce = cell.Nonce
		for i := nonceBytes; i > 0; i-- {
			buffer[pos+i] = byte(nonce)
			nonce >>= 8
		}
	}
	pos += 1 + nonceBytes

	// Encoding balance
	if cell.Balance.LtUint64(128) && !cell.Balance.IsZero() {
		buffer[pos] = byte(cell.Balance.Uint64())
		pos++
	} else {
		buffer[pos] = byte(128 + balanceBytes)
		pos++
		cell.Balance.WriteToSlice(buffer[pos : pos+balanceBytes])
		pos += balanceBytes
	}

	// Encoding Root and CodeHash
	buffer[pos] = 128 + 32
	pos++
	copy(buffer[pos:], storageRootHash[:])
	pos += 32
	buffer[pos] = 128 + 32
	pos++
	copy(buffer[pos:], cell.CodeHash[:])
	pos += 32
	return pos
}

func (hph *HexPatriciaHashed) completeLeafHash(buf []byte, compactLen int, key []byte, compact0 byte, ni int, val rlp.RlpSerializable, singleton bool) ([]byte, error) {
	// Compute the total length of binary representation
	var kp, kl int
	var keyPrefix [1]byte
	if compactLen > 1 {
		keyPrefix[0] = 0x80 + byte(compactLen)
		kp = 1
		kl = compactLen
	} else {
		kl = 1
	}

	totalLen := kp + kl + val.DoubleRLPLen()
	var lenPrefix [4]byte
	pl := rlp.EncodeListPrefixToBuf(totalLen, lenPrefix[:])
	canEmbed := !singleton && totalLen+pl < length.Hash
	var writer io.Writer
	if canEmbed {
		//hph.byteArrayWriter.Setup(buf)
		hph.auxBuffer.Reset()
		writer = hph.auxBuffer
	} else {
		hph.keccak.Reset()
		writer = hph.keccak
	}
	if _, err := writer.Write(lenPrefix[:pl]); err != nil {
		return nil, err
	}
	if _, err := writer.Write(keyPrefix[:kp]); err != nil {
		return nil, err
	}
	b := [1]byte{compact0}
	if _, err := writer.Write(b[:]); err != nil {
		return nil, err
	}
	for i := 1; i < compactLen; i++ {
		b[0] = key[ni]*16 + key[ni+1]
		if _, err := writer.Write(b[:]); err != nil {
			return nil, err
		}
		ni += 2
	}
	var prefixBuf [8]byte
	if err := val.ToDoubleRLP(writer, prefixBuf[:]); err != nil {
		return nil, err
	}
	if canEmbed {
		buf = hph.auxBuffer.Bytes()
	} else {
		var hashBuf [33]byte
		hashBuf[0] = 0x80 + length.Hash
		if _, err := hph.keccak.Read(hashBuf[1:]); err != nil {
			return nil, err
		}
		buf = append(buf, hashBuf[:]...)
	}
	return buf, nil
}

func (hph *HexPatriciaHashed) leafHashWithKeyVal(buf, key []byte, val rlp.RlpSerializableBytes, singleton bool) ([]byte, error) {
	// Write key
	var compactLen int
	var ni int
	var compact0 byte
	compactLen = (len(key)-1)/2 + 1
	if len(key)&1 == 0 {
		compact0 = 0x30 + key[0] // Odd: (3<<4) + first nibble
		ni = 1
	} else {
		compact0 = 0x20
	}
	return hph.completeLeafHash(buf, compactLen, key, compact0, ni, val, singleton)
}

func (hph *HexPatriciaHashed) accountLeafHashWithKey(buf, key []byte, val rlp.RlpSerializable) ([]byte, error) {
	// Write key
	var compactLen int
	var ni int
	var compact0 byte
	if nibbles.HasTerm(key) {
		compactLen = (len(key)-1)/2 + 1
		if len(key)&1 == 0 {
			compact0 = 48 + key[0] // Odd (1<<4) + first nibble
			ni = 1
		} else {
			compact0 = 32
		}
	} else {
		compactLen = len(key)/2 + 1
		if len(key)&1 == 1 {
			compact0 = terminatorHexByte + key[0] // Odd (1<<4) + first nibble
			ni = 1
		}
	}
	return hph.completeLeafHash(buf, compactLen, key, compact0, ni, val, true)
}

func (hph *HexPatriciaHashed) extensionHash(key []byte, hash []byte) (common.Hash, error) {
	var hashBuf common.Hash

	// Compute the total length of binary representation
	var kp, kl int
	// Write key
	var compactLen int
	var ni int
	var compact0 byte
	if nibbles.HasTerm(key) {
		compactLen = (len(key)-1)/2 + 1
		if len(key)&1 == 0 {
			compact0 = 0x30 + key[0] // Odd: (3<<4) + first nibble
			ni = 1
		} else {
			compact0 = 0x20
		}
	} else {
		compactLen = len(key)/2 + 1
		if len(key)&1 == 1 {
			compact0 = 0x10 + key[0] // Odd: (1<<4) + first nibble
			ni = 1
		}
	}
	var keyPrefix [1]byte
	if compactLen > 1 {
		keyPrefix[0] = 0x80 + byte(compactLen)
		kp = 1
		kl = compactLen
	} else {
		kl = 1
	}
	totalLen := kp + kl + 33
	var lenPrefix [4]byte
	pt := rlp.EncodeListPrefixToBuf(totalLen, lenPrefix[:])
	hph.keccak.Reset()
	if _, err := hph.keccak.Write(lenPrefix[:pt]); err != nil {
		return hashBuf, err
	}
	if _, err := hph.keccak.Write(keyPrefix[:kp]); err != nil {
		return hashBuf, err
	}
	var b [1]byte
	b[0] = compact0
	if _, err := hph.keccak.Write(b[:]); err != nil {
		return hashBuf, err
	}
	for i := 1; i < compactLen; i++ {
		b[0] = key[ni]*16 + key[ni+1]
		if _, err := hph.keccak.Write(b[:]); err != nil {
			return hashBuf, err
		}
		ni += 2
	}
	b[0] = 0x80 + length.Hash
	if _, err := hph.keccak.Write(b[:]); err != nil {
		return hashBuf, err
	}
	if _, err := hph.keccak.Write(hash); err != nil {
		return hashBuf, err
	}
	// Replace previous hash with the new one
	if _, err := hph.keccak.Read(hashBuf[:]); err != nil {
		return hashBuf, err
	}
	return hashBuf, nil
}

func (hph *HexPatriciaHashed) computeCellHashLen(cell *cell, depth int16) int16 {
	if cell.storageAddrLen > 0 && depth >= 64 {
		if cell.stateHashLen > 0 {
			return cell.stateHashLen + 1
		}

		keyLen := 128 - depth + 1 // Length of hex key with terminator character
		var kp, kl int
		compactLen := (keyLen-1)/2 + 1
		if compactLen > 1 {
			kp = 1
			kl = int(compactLen)
		} else {
			kl = 1
		}
		val := rlp.RlpSerializableBytes(cell.Storage[:cell.StorageLen])
		totalLen := kp + kl + val.DoubleRLPLen()
		var lenPrefix [4]byte
		pt := rlp.EncodeListPrefixToBuf(totalLen, lenPrefix[:])
		if totalLen+pt < length.Hash {
			return int16(totalLen + pt)
		}
	}
	return length.Hash + 1
}

func (hph *HexPatriciaHashed) computeCellHash(cell *cell, depth int16, buf []byte) ([]byte, error) {
	var err error
	var storageRootHash common.Hash
	var storageRootHashIsSet bool
	if hph.memoizationOff {
		cell.stateHashLen = 0 // Reset stateHashLen to force recompute
	}
	if cell.storageAddrLen > 0 {
		var hashedKeyOffset int16
		if depth >= 64 {
			hashedKeyOffset = depth - 64
		}
		singleton := depth <= 64

		// Check cached stateHash BEFORE hashing key (optimization: skip key hash if using cache)
		if cell.stateHashLen > 0 {
			if hph.trace {
				fmt.Printf("REUSED stateHash %x spk %x\n", cell.stateHash[:cell.stateHashLen], cell.storageAddr[:cell.storageAddrLen])
			}
			mxTrieStateSkipRate.Inc()
			skippedLoad.Add(1)
			if !singleton {
				return append(append(buf[:0], byte(160)), cell.stateHash[:cell.stateHashLen]...), nil
			}
			storageRootHashIsSet = true
			storageRootHash = *(*common.Hash)(cell.stateHash[:cell.stateHashLen])
		} else {
			koffset := hph.accountKeyLen
			if depth == 0 && cell.accountAddrLen == 0 {
				// if account key is empty, then we need to hash storage key from the key beginning
				koffset = 0
			}
			if err = cell.hashStorageKey(hph.keccak, koffset, 0, hashedKeyOffset, hph.cellHashBuf[:]); err != nil {
				return nil, err
			}
			cell.hashedExtension[64-hashedKeyOffset] = terminatorHexByte // Add terminator
			if !cell.loaded.storage() {
				return nil, fmt.Errorf("storage %x was not loaded as expected: cell %v", cell.storageAddr[:cell.storageAddrLen], cell.String())
				// update, err := hph.storageFromCacheOrDB(cell.storageAddr[:cell.storageAddrLen])
				// if err != nil {
				// 	return nil, err
				// }
				// cell.setFromUpdate(update)
			}

			leafHash, err := hph.leafHashWithKeyVal(buf, cell.hashedExtension[:64-hashedKeyOffset+1], cell.Storage[:cell.StorageLen], singleton)
			if err != nil {
				return nil, err
			}
			if hph.trace {
				fmt.Printf("leafHashWithKeyVal(singleton=%t) {%x} for [%x]=>[%x] %v\n",
					singleton, leafHash, cell.hashedExtension[:64-hashedKeyOffset+1], cell.Storage[:cell.StorageLen], cell.String())
			}
			if !singleton {
				copy(cell.stateHash[:], leafHash[1:])
				cell.stateHashLen = int16(len(leafHash) - 1)
				return leafHash, nil
			}
			storageRootHash = *(*common.Hash)(leafHash[1:])
			storageRootHashIsSet = true
			cell.stateHashLen = 0
			hadToReset.Add(1)
		}
	}
	if cell.accountAddrLen > 0 {
		if err := cell.hashAccKey(hph.keccak, depth, hph.cellHashBuf[:]); err != nil {
			return nil, err
		}
		cell.hashedExtension[64-depth] = terminatorHexByte // Add terminator
		if !storageRootHashIsSet {
			if cell.extLen > 0 { // Extension
				if cell.hashLen == 0 {
					return nil, errors.New("computeCellHash extension without hash")
				}
				if hph.trace {
					fmt.Printf("extensionHash for [%x]=>[%x]\n", cell.extension[:cell.extLen], cell.hash[:cell.hashLen])
				}
				if storageRootHash, err = hph.extensionHash(cell.extension[:cell.extLen], cell.hash[:cell.hashLen]); err != nil {
					return nil, err
				}
				if hph.trace {
					fmt.Printf("EXTENSION HASH %x DROPS stateHash\n", storageRootHash)
				}
				cell.stateHashLen = 0
				hadToReset.Add(1)
			} else if cell.hashLen > 0 {
				storageRootHash = cell.hash
			} else {
				storageRootHash = empty.RootHash
			}
		}
		if !cell.loaded.account() {
			if cell.stateHashLen > 0 {
				hph.keccak.Reset()

				mxTrieStateSkipRate.Inc()
				skippedLoad.Add(1)
				if hph.trace {
					fmt.Printf("REUSED stateHash %x apk %x\n", cell.stateHash[:cell.stateHashLen], cell.accountAddr[:cell.accountAddrLen])
				}
				return append(append(buf[:0], byte(160)), cell.stateHash[:cell.stateHashLen]...), nil
			}
			// storage root update or extension update could invalidate older stateHash, so we need to reload state
			hph.metrics.AccountLoad(cell.accountAddr[:cell.accountAddrLen])
			update, err := hph.accountFromCacheOrDB(cell.accountAddr[:cell.accountAddrLen])
			if err != nil {
				return nil, err
			}
			cell.setFromUpdate(update)
		}

		valLen := cell.accountForHashing(hph.accValBuf, storageRootHash)
		buf, err = hph.accountLeafHashWithKey(buf, cell.hashedExtension[:65-depth], hph.accValBuf[:valLen])
		if err != nil {
			return nil, err
		}
		if hph.trace {
			fmt.Printf("accountLeafHashWithKey {%x} (memorised) for [%x]=>[%x]\n", buf, cell.hashedExtension[:65-depth], hph.accValBuf[:valLen])
		}
		copy(cell.stateHash[:], buf[1:])
		cell.stateHashLen = int16(len(buf)) - 1
		return buf, nil
	}

	buf = append(buf, 0x80+32)
	if cell.extLen > 0 { // Extension
		if cell.hashLen > 0 {
			if hph.trace {
				fmt.Printf("extensionHash for [%x]=>[%x]\n", cell.extension[:cell.extLen], cell.hash[:cell.hashLen])
			}
			if storageRootHash, err = hph.extensionHash(cell.extension[:cell.extLen], cell.hash[:cell.hashLen]); err != nil {
				return nil, err
			}
			buf = append(buf, storageRootHash[:]...)
		} else {
			return nil, errors.New("computeCellHash extension without hash")
		}
	} else if cell.hashLen > 0 {
		buf = append(buf, cell.hash[:cell.hashLen]...)
	} else if storageRootHashIsSet {
		buf = append(buf, storageRootHash[:]...)
		copy(cell.hash[:], storageRootHash[:])
		cell.hashLen = int16(len(storageRootHash))
	} else {
		buf = append(buf, emptyRootHashBytes...)
	}
	return buf, nil
}

func (hph *HexPatriciaHashed) needUnfolding(hashedKey []byte) int16 {
	var cell *cell
	var depth int16
	if hph.activeRows == 0 {
		if hph.trace {
			fmt.Printf("needUnfolding root, rootChecked = %t\n", hph.rootChecked)
		}
		if hph.root.hashedExtLen == 64 && hph.root.accountAddrLen > 0 && hph.root.storageAddrLen > 0 {
			// in case if root is a leaf node with storage and account, we need to derive storage part of a key
			if err := hph.root.deriveHashedKeys(depth, hph.keccak, hph.accountKeyLen, hph.cellHashBuf[:]); err != nil {
				log.Warn("deriveHashedKeys for root with storage", "err", err, "cell", hph.root.FullString())
				return 0
			}
			if hph.trace {
				fmt.Printf("derived prefix %x\n", hph.currentKey[:hph.currentKeyLen])
			}
		}
		if hph.root.hashedExtLen == 0 && hph.root.hashLen == 0 {
			if hph.rootChecked {
				return 0 // Previously checked, empty root, no unfolding needed
			}
			return 1 // Need to attempt to unfold the root
		}
		cell = &hph.root
	} else {
		nibble := int(hashedKey[hph.currentKeyLen])
		cell = &hph.grid[hph.activeRows-1][nibble]
		depth = hph.depths[hph.activeRows-1]
		if hph.trace {
			fmt.Printf("currentKey [%x] needUnfolding cell (%d, %x, depth=%d) cell.hash=[%x]\n", hph.currentKey[:hph.currentKeyLen], hph.activeRows-1, nibble, depth, cell.hash[:cell.hashLen])
		}
	}
	if int16(len(hashedKey)) <= depth {
		return 0
	}
	if cell.hashedExtLen == 0 {
		if cell.hashLen == 0 { // cell is empty, no need to unfold further
			return 0
		}
		return 1 // unfold branch node
	}

	cpl := nibbles.CommonPrefixLen(hashedKey[depth:], cell.hashedExtension[:cell.hashedExtLen-1])
	if hph.trace {
		fmt.Printf("cpl=%d cell.hashedExtension=[%x] hashedKey[depth=%d:]=[%x]\n", cpl, cell.hashedExtension[:cell.hashedExtLen], depth, hashedKey[depth:])
	}
	unfolding := int16(cpl + 1)
	if depth < 64 && depth+unfolding > 64 {
		// This is to make sure that unfolding always breaks at the level where storage subtrees start
		unfolding = 64 - depth
		if hph.trace {
			fmt.Printf("adjusted unfolding=%d <- %d\n", unfolding, cpl+1)
		}
	}
	return unfolding
}

func (c *cell) IsEmpty() bool {
	return c == nil || (c.hashLen == 0 && c.hashedExtLen == 0 && c.extLen == 0 && c.accountAddrLen == 0 && c.storageAddrLen == 0)
}

func (c *cell) String() string {
	var s strings.Builder
	s.WriteString("(")
	if c.hashLen > 0 {
		s.WriteString(fmt.Sprintf("hash(len=%d)=%x, ", c.hashLen, c.hash))
	}
	if c.hashedExtLen > 0 {
		s.WriteString(fmt.Sprintf("hashedExtension(len=%d)=%x, ", c.hashedExtLen, c.hashedExtension[:c.hashedExtLen]))
	}
	if c.extLen > 0 {
		s.WriteString(fmt.Sprintf("extension(len=%d)=%x, ", c.extLen, c.extension[:c.extLen]))
	}
	if c.accountAddrLen > 0 {
		s.WriteString(fmt.Sprintf("accountAddr=%x, ", c.accountAddr))
	}
	if c.storageAddrLen > 0 {
		s.WriteString(fmt.Sprintf("storageAddr=%x, ", c.storageAddr))
	}

	s.WriteString(")")
	return s.String()
}

// readBranchAndCheckForFlushing reads a branch from ctx, flushing deferred updates first if the prefix is pending.
// This ensures we read fresh data when a prefix has been modified but not yet written.
func (hph *HexPatriciaHashed) readBranchAndCheckForFlushing(prefix []byte) ([]byte, error) {
	be := hph.branchEncoder
	if be.DeferUpdatesEnabled() && be.HasPendingPrefix(prefix) {
		if err := be.ApplyDeferredUpdates(16, hph.ctx.PutBranch); err != nil {
			return nil, err
		}
		be.ClearDeferred()
	}
	return hph.branchFromCacheOrDB(prefix)
}

// unfoldBranchNode returns true if unfolding has been done
func (hph *HexPatriciaHashed) unfoldBranchNode(row int, depth int16, deleted bool) error {
	key := nibbles.HexToCompact(hph.currentKey[:hph.currentKeyLen])
	hph.metrics.BranchLoad(hph.currentKey[:hph.currentKeyLen])

	branchData, err := hph.readBranchAndCheckForFlushing(key)
	if err != nil {
		return err
	}

	// depthsToTxNum is used for per-file metrics; step is no longer available
	// from the cache-or-DB helper (cache never had a meaningful step anyway).
	hph.depthsToTxNum[depth] = 0

	if len(branchData) >= 2 {
		branchData = branchData[2:] // skip touch map and keep the rest
	}
	if hph.trace {
		fmt.Printf("unfoldBranchNode prefix '%x', nibbles [%x] depth %d row %d '%x'\n",
			key, hph.currentKey[:hph.currentKeyLen], depth, row, branchData)
	}
	if !hph.rootChecked && hph.currentKeyLen == 0 && len(branchData) == 0 {
		// Special case - empty or deleted root
		hph.rootChecked = true
		return nil
	}
	if len(branchData) == 0 {
		log.Warn("got empty branch data during unfold", "key", hex.EncodeToString(key), "row", row, "depth", depth, "deleted", deleted)
		if hph.trace {
			branchData, _ = hph.branchFromCacheOrDB(key)
			fmt.Printf("unfoldBranchNode prefix '%x', nibbles [%x] depth %d row %d '%x' %s\n", key, hph.currentKey[:hph.currentKeyLen], depth, row, branchData, BranchData(branchData).String())
		}
		return fmt.Errorf("empty branch data read during unfold, compact prefix %x nibbles %x", key, hph.currentKey[:hph.currentKeyLen])
	}
	hph.branchBefore[row] = true
	bitmap := binary.BigEndian.Uint16(branchData[0:])
	pos := 2
	if deleted { // All cells come as deleted (touched but not present after)
		hph.touchMap[row], hph.afterMap[row] = bitmap, 0
	} else {
		hph.touchMap[row], hph.afterMap[row] = 0, bitmap
	}
	//fmt.Printf("unfoldBranchNode prefix '%x' [%x], afterMap = [%016b], touchMap = [%016b]\n", key, branchData, hph.afterMap[row], hph.touchMap[row])
	// Loop iterating over the set bits of modMask
	for bitset, j := bitmap, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		cell := &hph.grid[row][nibble]
		fieldBits := branchData[pos]
		pos++
		if pos, err = cell.fillFromFields(branchData, pos, cellFields(fieldBits)); err != nil {
			return fmt.Errorf("prefix [%x] branchData[%x]: %w", hph.currentKey[:hph.currentKeyLen], branchData, err)
		}
		if hph.trace {
			fmt.Printf("cell (%d, %x, depth=%d) %s\n", row, nibble, depth, cell.FullString())
		}

		// relies on plain account/storage key so need to be dereferenced before hashing
		if err = cell.deriveHashedKeys(depth, hph.keccak, hph.accountKeyLen, hph.cellHashBuf[:]); err != nil {
			return err
		}
		bitset ^= bit
	}
	hph.depths[hph.activeRows] = depth
	hph.activeRows++
	return nil
}

func (hph *HexPatriciaHashed) unfold(hashedKey []byte, unfolding int16) error {
	if hph.trace {
		fmt.Printf("unfold %d: activeRows: %d\n", unfolding, hph.activeRows)
	}
	var upCell *cell
	var touched, present bool
	var upDepth, depth int16
	if hph.activeRows == 0 {
		if hph.rootChecked && hph.root.hashLen == 0 && hph.root.hashedExtLen == 0 {
			return nil // No unfolding for empty root
		}
		upCell = &hph.root
		touched = hph.rootTouched
		present = hph.rootPresent
		if hph.trace {
			fmt.Printf("unfold root: touched: %t present: %t %s\n", touched, present, upCell.FullString())
		}
	} else {
		upRow := hph.activeRows - 1
		upDepth = hph.depths[upRow]
		upNibble := hashedKey[upDepth-1]
		upCell = &hph.grid[upRow][upNibble]

		touched = hph.touchMap[upRow]&(uint16(1)<<upNibble) != 0
		present = hph.afterMap[upRow]&(uint16(1)<<upNibble) != 0
		if hph.trace {
			fmt.Printf("upCell (%d, %x, updepth=%d) touched: %t present: %t\n", upRow, upNibble, upDepth, touched, present)
		}
		hph.currentKey[hph.currentKeyLen] = upNibble
		hph.currentKeyLen++
	}
	row := hph.activeRows
	for i := 0; i < 16; i++ {
		hph.grid[row][i].reset()
	}
	hph.touchMap[row], hph.afterMap[row], hph.branchBefore[row] = 0, 0, false

	if upCell.hashedExtLen == 0 {
		depth = upDepth + 1
		return hph.unfoldBranchNode(row, depth, touched && !present)
	}

	lowest := min(unfolding, upCell.hashedExtLen)
	depth = upDepth + lowest
	copyLen := lowest - 1
	nibble := upCell.hashedExtension[copyLen]

	if touched {
		hph.touchMap[row] = uint16(1) << nibble
	}
	if present {
		hph.afterMap[row] = uint16(1) << nibble
	}

	cell := &hph.grid[row][nibble]
	cell.fillFromUpperCell(upCell, depth, lowest)
	if hph.trace {
		fmt.Printf("unfolded cell (%d, %x, depth=%d) %s\n", row, nibble, depth, cell.FullString())
	}
	if row >= 64 {
		cell.accountAddrLen = 0
	}

	if copyLen > 0 {
		copy(hph.currentKey[hph.currentKeyLen:], upCell.hashedExtension[:copyLen])
		hph.currentKeyLen += copyLen
	}

	hph.depths[hph.activeRows] = depth
	hph.activeRows++
	return nil
}

func (hph *HexPatriciaHashed) needFolding(hashedKey []byte) bool {
	return !bytes.HasPrefix(hashedKey, hph.currentKey[:hph.currentKeyLen])
}

var (
	hadToLoad   atomic.Uint64
	skippedLoad atomic.Uint64
	hadToReset  atomic.Uint64
)

type skipStat struct {
	accLoaded, accSkipped, accReset, storReset, storLoaded, storSkipped uint64
}

const terminatorHexByte = 16 // max nibble value +1. Defines end of nibble line in the trie or splits address and storage space in trie.

// updateKind is a type of update that is being applied to the trie structure.
type updateKind uint8

const (
	// updateKindDelete means after we processed longest common prefix, row ended up empty.
	updateKindDelete updateKind = 0b0

	// updateKindPropagate is an update operation ended up with a single nibble which is leaf or extension node.
	// We do not store keys with only one cell as a value in db, instead we copy them upwards to the parent branch.
	//
	// In case current prefix existed before and node is fused to upper level, this causes deletion for current prefix
	// and update of branch value on upper level.
	// 	e.g.: leaf was at prefix 0xbeef, but we fuse it in level above, so
	//  - delete 0xbeef
	//  - update 0xbee
	updateKindPropagate updateKind = 0b01

	// updateKindBranch is an update operation ended up as a branch of 2+ cells.
	// That does not necessarily means that branch is NEW, it could be an existing branch that was updated.
	updateKindBranch updateKind = 0b10
)

// Kind defines how exactly given update should be folded upwards to the parent branch or root.
// It also returns number of nibbles that left in branch after the operation.
func afterMapUpdateKind(afterMap uint16) (kind updateKind, nibblesAfterUpdate int) {
	nibblesAfterUpdate = bits.OnesCount16(afterMap)
	switch nibblesAfterUpdate {
	case 0:
		return updateKindDelete, nibblesAfterUpdate
	case 1:
		return updateKindPropagate, nibblesAfterUpdate
	default:
		return updateKindBranch, nibblesAfterUpdate
	}
}

// foldBranch handles the updateKindBranch case: branch of 2+ cells.
func (hph *HexPatriciaHashed) foldBranch(row int, nibble, upDepth, depth int16, upCell *cell, updateKey []byte) error {
	if hph.touchMap[row] != 0 { // any modifications
		if row == 0 {
			hph.rootTouched = true
			hph.rootPresent = true
		} else {
			// Modification is propagated upwards
			hph.touchMap[row-1] |= uint16(1) << nibble
		}
	}
	bitmap := hph.touchMap[row] & hph.afterMap[row]
	if !hph.branchBefore[row] {
		// There was no branch node before, so we need to touch even the singular child that existed
		hph.touchMap[row] |= hph.afterMap[row]
		bitmap |= hph.afterMap[row]
	}

	// Calculate total length of all hashes
	nibblesLeftAfterUpdate := bits.OnesCount16(hph.afterMap[row])
	totalBranchLen, err := hph.prepareBranchCells(row, depth, nibblesLeftAfterUpdate)
	if err != nil {
		return err
	}

	hph.keccak2.Reset()
	pt := rlp.EncodeListPrefixToBuf(int(totalBranchLen), hph.hashAuxBuffer[:])
	if _, err := hph.keccak2.Write(hph.hashAuxBuffer[:pt]); err != nil {
		return err
	}

	// Single pass: feed keccak2 + extract cellEncodeData
	cellData, err := hph.hashRow(row, depth)
	if err != nil {
		return err
	}

	if hph.branchEncoder.DeferUpdatesEnabled() {
		if err := hph.branchEncoder.CollectDeferredUpdate(hph.ctx, updateKey, bitmap, hph.touchMap[row], hph.afterMap[row], &cellData); err != nil {
			return fmt.Errorf("failed to collect deferred branch update: %w", err)
		}
	} else {
		if err := hph.branchEncoder.CollectUpdate(hph.ctx, updateKey, bitmap, hph.touchMap[row], hph.afterMap[row], &cellData); err != nil {
			return fmt.Errorf("failed to encode branch update: %w", err)
		}
	}
	upCell.extLen = depth - upDepth - 1
	upCell.hashedExtLen = upCell.extLen
	if upCell.extLen > 0 {
		copy(upCell.extension[:], hph.currentKey[upDepth:hph.currentKeyLen])
		copy(upCell.hashedExtension[:], hph.currentKey[upDepth:hph.currentKeyLen])
	}
	if depth < 64 {
		upCell.accountAddrLen = 0
	}
	upCell.storageAddrLen = 0
	upCell.hashLen = 32
	if _, err := hph.keccak2.Read(upCell.hash[:]); err != nil {
		return err
	}
	if hph.trace {
		fmt.Printf("} [%x]\n", upCell.hash[:])
	}
	return nil
}

// hashRow performs a single pass over all 17 branch slots (16 nibbles + terminator),
// feeding cell hashes to keccak2 for present cells (per afterMap) and writing 0x80
// for empty slots. It simultaneously extracts cellEncodeData for each present cell.
// This replaces the separate feedBranchHashesToKeccak + cellEncodeData extraction loop.
func (hph *HexPatriciaHashed) hashRow(row int, depth int16) ([16]cellEncodeData, error) {
	var cellData [16]cellEncodeData
	b := [...]byte{0x80}

	for bitset, lastNib := hph.afterMap[row], 0; ; {
		if bitset == 0 {
			// Write remaining empty cells to keccak2 (up to slot 16 inclusive = terminator)
			for i := lastNib; i < 17; i++ {
				if _, err := hph.keccak2.Write(b[:]); err != nil {
					return cellData, err
				}
			}
			break
		}
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)

		// Write empty cells before this nibble
		for i := lastNib; i < nibble; i++ {
			if _, err := hph.keccak2.Write(b[:]); err != nil {
				return cellData, err
			}
			if hph.trace {
				fmt.Printf("  %x: empty(%d, %x, depth=%d)\n", i, row, i, depth)
			}
		}
		lastNib = nibble + 1

		cell := &hph.grid[row][nibble]

		// Warn about unloaded state
		if cell.accountAddrLen > 0 && cell.stateHashLen == 0 && !cell.loaded.account() && !cell.Deleted() {
			log.Warn("account not loaded", "row", row, "nibble", fmt.Sprintf("%x", nibble), "depth", depth, "cell", cell.String())
		}
		if cell.storageAddrLen > 0 && cell.stateHashLen == 0 && !cell.loaded.storage() && !cell.Deleted() {
			log.Warn("storage not loaded", "row", row, "nibble", fmt.Sprintf("%x", nibble), "depth", depth, "cell", cell.String())
		}

		// Save hash before compute for metrics
		var hashBefore []byte
		if dbg.KVReadLevelledMetrics && (cell.accountAddrLen > 0 || cell.storageAddrLen > 0) {
			hashBefore = make([]byte, cell.stateHashLen)
			copy(hashBefore, cell.stateHash[:cell.stateHashLen])
		}
		loadedBefore := cell.loaded

		cellHash, err := hph.computeCellHash(cell, depth, hph.hashAuxBuffer[:0])
		if err != nil {
			return cellData, err
		}
		if hph.trace {
			fmt.Printf("  %x: computeCellHash(%d, %x, depth=%d)=[%x]\n", nibble, row, nibble, depth, cellHash)
		}

		// Collect metrics on hash recomputation vs skip
		if dbg.KVReadLevelledMetrics && hashBefore != nil {
			counters := hph.hadToLoadL[hph.depthsToTxNum[depth]]
			if !bytes.Equal(hashBefore, cell.stateHash[:cell.stateHashLen]) {
				if cell.accountAddrLen > 0 {
					counters.accReset++
					counters.accLoaded++
				}
				if cell.storageAddrLen > 0 {
					counters.storReset++
					counters.storLoaded++
				}
			} else {
				if cell.accountAddrLen > 0 && !loadedBefore.account() && !cell.loaded.account() {
					counters.accSkipped++
				}
				if cell.storageAddrLen > 0 && !loadedBefore.storage() && !cell.loaded.storage() {
					counters.storSkipped++
				}
			}
			hph.hadToLoadL[hph.depthsToTxNum[depth]] = counters
		}

		if _, err := hph.keccak2.Write(cellHash); err != nil {
			return cellData, err
		}

		// Extract encoding data
		cellData[nibble] = cellEncodeDataFromCell(cell)

		bitset ^= bit
	}
	return cellData, nil
}

// prepareBranchCells iterates afterMap cells, drops stale memoized hashes,
// loads state from DB where needed, and returns the total RLP-encoded branch length.
func (hph *HexPatriciaHashed) prepareBranchCells(row int, depth int16, nibblesLeftAfterUpdate int) (int16, error) {
	totalBranchLen := int16(17 - nibblesLeftAfterUpdate) // For every empty cell, one byte
	for bitset, j := hph.afterMap[row], 0; bitset != 0; j++ {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		cell := &hph.grid[row][nibble]

		if hph.memoizationOff {
			cell.stateHashLen = 0
		}
		/* memoization of state hashes*/
		var counters skipStat
		if dbg.KVReadLevelledMetrics {
			counters = hph.hadToLoadL[hph.depthsToTxNum[depth]]
		}
		if cell.stateHashLen > 0 && (hph.touchMap[row]&hph.afterMap[row]&uint16(1<<nibble) > 0 || cell.stateHashLen != length.Hash) {
			// drop state hash if updated or hashLen < 32 (corner case, may even not encode such leaf hashes)
			if hph.trace {
				fmt.Printf("DROP hash for (%d, %x, depth=%d) %s\n", row, nibble, depth, cell.FullString())
			}
			cell.stateHashLen = 0
			hadToReset.Add(1)
			if cell.accountAddrLen > 0 {
				counters.accReset++
			}
			if cell.storageAddrLen > 0 {
				counters.storReset++
			}
		}
		var err error
		counters, err = hph.loadStateIfNeeded(cell, counters)
		if err != nil {
			return 0, err
		}
		if dbg.KVReadLevelledMetrics {
			hph.hadToLoadL[hph.depthsToTxNum[depth]] = counters
		}
		/* end of memoization */

		totalBranchLen += hph.computeCellHashLen(cell, depth)
		bitset ^= bit
	}
	return totalBranchLen, nil
}

// foldPropagate handles the updateKindPropagate case: leaf or extension node.
func (hph *HexPatriciaHashed) foldPropagate(row int, nibble, upDepth, depth int16, upCell *cell, updateKey []byte) error {
	if hph.touchMap[row] != 0 {
		// any modifications
		if row == 0 {
			hph.rootTouched = true
		} else {
			// Modification is propagated upwards
			hph.touchMap[row-1] |= uint16(1) << nibble
		}
	}
	childNibble := bits.TrailingZeros16(hph.afterMap[row])
	cell := &hph.grid[row][childNibble]
	upCell.extLen = 0
	upCell.stateHashLen = 0
	var counters skipStat
	if dbg.KVReadLevelledMetrics {
		counters = hph.hadToLoadL[hph.depthsToTxNum[depth]]
	}
	counters, err := hph.loadStateIfNeeded(cell, counters)
	if err != nil {
		return err
	}
	if dbg.KVReadLevelledMetrics {
		hph.hadToLoadL[hph.depthsToTxNum[depth]] = counters
	}
	// propagate cell into parent row
	upCell.fillFromLowerCell(cell, depth, hph.currentKey[upDepth:hph.currentKeyLen], childNibble)

	if err := hph.collectDeleteUpdate(updateKey, row); err != nil {
		return err
	}
	if hph.trace {
		fmt.Printf("formed leaf (%d %x, depth=%d) [%x] %s\n", row, childNibble, depth, updateKey, cell.FullString())
	}
	return nil
}

// foldDelete handles the updateKindDelete case: everything at this row was deleted.
func (hph *HexPatriciaHashed) foldDelete(row int, nibble, upDepth int16, upCell *cell, updateKey []byte) error {
	if hph.touchMap[row] != 0 {
		if row == 0 {
			// Root is deleted because the tree is empty
			hph.rootTouched = true
			hph.rootPresent = false
		} else if upDepth == 64 {
			// Special case - all storage items of an account have been deleted, but it does not automatically delete the account, just makes it empty storage
			// Therefore we are not propagating deletion upwards, but turn it into a modification
			hph.touchMap[row-1] |= uint16(1) << nibble
		} else {
			// Deletion is propagated upwards
			hph.touchMap[row-1] |= uint16(1) << nibble
			hph.afterMap[row-1] &^= uint16(1) << nibble
		}
	}

	upCell.reset()
	return hph.collectDeleteUpdate(updateKey, row)
}

// collectDeleteUpdate encodes a branch deletion if a branch existed before at this row.
// If evictCache is true, it also evicts the branch from the cache.
func (hph *HexPatriciaHashed) collectDeleteUpdate(updateKey []byte, row int) error {
	if hph.branchBefore[row] {
		if err := hph.branchEncoder.CollectUpdate(hph.ctx, updateKey, 0, hph.touchMap[row], 0, nil); err != nil {
			return fmt.Errorf("failed to encode leaf node update: %w", err)
		}
	}
	return nil
}

// The purpose of fold is to reduce hph.currentKey[:hph.currentKeyLen]. It should be invoked
// until that current key becomes a prefix of hashedKey that we will process next
// (in other words until the needFolding function returns 0)
func (hph *HexPatriciaHashed) fold() error {
	updateKeyLen := hph.currentKeyLen
	if hph.activeRows == 0 {
		return errors.New("cannot fold - no active rows")
	}
	if hph.trace {
		fmt.Printf("fold [%x] activeRows: %d touchMap: %016b afterMap: %016b\n", hph.currentKey[:hph.currentKeyLen], hph.activeRows, hph.touchMap[hph.activeRows-1], hph.afterMap[hph.activeRows-1])
	}
	// Move information to the row above
	var upCell *cell
	var nibble, upDepth int16
	row := hph.activeRows - 1
	upRow := row - 1
	if row == 0 {
		if hph.trace {
			fmt.Printf("fold: parent is root %s\n", hph.root.FullString())
		}
		upCell = &hph.root
	} else {
		upDepth = hph.depths[upRow]
		nibble = int16(hph.currentKey[upDepth-1])
		if hph.trace {
			fmt.Printf("fold: parent (%d, %x, depth=%d)\n", upRow, nibble, upDepth)
		}
		upCell = &hph.grid[upRow][nibble]
	}

	depth := hph.depths[row]

	updateKey := nibbles.HexToCompact(hph.currentKey[:updateKeyLen])
	defer func() { hph.depthsToTxNum[depth] = 0 }()

	if hph.trace {
		fmt.Printf("fold: (row=%d, {%s}, depth=%d) prefix [%x] touchMap: %016b afterMap: %016b \n",
			row, updatedNibs(hph.touchMap[row]&hph.afterMap[row]), depth, hph.currentKey[:hph.currentKeyLen], hph.touchMap[row], hph.afterMap[row])
	}

	updateKind, _ := afterMapUpdateKind(hph.afterMap[row])
	var err error
	switch updateKind {
	case updateKindDelete: // Everything deleted
		err = hph.foldDelete(row, nibble, upDepth, upCell, updateKey)
	case updateKindPropagate: // Leaf or extension node
		err = hph.foldPropagate(row, nibble, upDepth, depth, upCell, updateKey)
	case updateKindBranch:
		err = hph.foldBranch(row, nibble, upDepth, depth, upCell, updateKey)
	}
	if err != nil {
		return err
	}

	hph.activeRows--
	hph.currentKeyLen = max(upDepth-1, 0)
	return nil
}

func (hph *HexPatriciaHashed) loadStateIfNeeded(cell *cell, counters skipStat) (skipStat, error) {
	if cell.stateHashLen == 0 {
		if !cell.loaded.account() && cell.accountAddrLen > 0 {
			hph.metrics.AccountLoad(cell.accountAddr[:cell.accountAddrLen])
			upd, err := hph.accountFromCacheOrDB(cell.accountAddr[:cell.accountAddrLen])
			if err != nil {
				return counters, err
			}
			cell.setFromUpdate(upd)
			// if the update is empty, the loaded flag was not updated so do it manually
			cell.loaded = cell.loaded.addFlag(cellLoadAccount)
			counters.accLoaded++
		}
		if !cell.loaded.storage() && cell.storageAddrLen > 0 {
			hph.metrics.StorageLoad(cell.storageAddr[:cell.storageAddrLen])
			upd, err := hph.storageFromCacheOrDB(cell.storageAddr[:cell.storageAddrLen])
			if err != nil {
				return counters, err
			}
			cell.setFromUpdate(upd)
			// if the update is empty, the loaded flag was not updated so do it manually
			cell.loaded = cell.loaded.addFlag(cellLoadStorage)
			counters.storLoaded++
		}
	}
	return counters, nil
}

func (hph *HexPatriciaHashed) deleteCell(hashedKey []byte) {
	if hph.trace {
		fmt.Printf("deleteCell, activeRows = %d\n", hph.activeRows)
	}
	var cell *cell
	if hph.activeRows == 0 { // Remove the root
		cell = &hph.root
		hph.rootTouched, hph.rootPresent = true, false
	} else {
		row := hph.activeRows - 1
		if hph.depths[row] < int16(len(hashedKey)) {
			if hph.trace {
				fmt.Printf("deleteCell skipping spurious delete depth=%d, len(hashedKey)=%d\n", hph.depths[row], len(hashedKey))
			}
			return
		}
		nibble := int(hashedKey[hph.currentKeyLen])
		cell = &hph.grid[row][nibble]
		col := uint16(1) << nibble
		if hph.afterMap[row]&col != 0 {
			// Prevent "spurious deletions", i.e. deletion of absent items
			hph.touchMap[row] |= col
			hph.afterMap[row] &^= col
			if hph.trace {
				fmt.Printf("deleteCell setting (%d, %x)\n", row, nibble)
			}
		} else {
			if hph.trace {
				fmt.Printf("deleteCell ignoring (%d, %x)\n", row, nibble)
			}
		}
	}
	cell.reset()
}

// fetches cell by key and set touch/after maps. Requires that prefix to be already unfolded
func (hph *HexPatriciaHashed) updateCell(plainKey, hashedKey []byte, u *Update) (cell *cell) {
	hph.metrics.Updates(plainKey)

	if u.Deleted() {
		hph.deleteCell(hashedKey)
		return nil
	}

	var depth int16
	if hph.activeRows == 0 {
		cell = &hph.root
		hph.rootTouched, hph.rootPresent = true, true
	} else {
		row := hph.activeRows - 1
		depth = hph.depths[row]
		nibble := int(hashedKey[hph.currentKeyLen])
		cell = &hph.grid[row][nibble]
		col := uint16(1) << nibble

		hph.touchMap[row] |= col
		hph.afterMap[row] |= col
		if hph.trace {
			fmt.Printf("updateCell setting (%d, %x, depth=%d)\n", row, nibble, depth)
		}
	}
	if cell.hashedExtLen == 0 {
		copy(cell.hashedExtension[:], hashedKey[depth:])
		cell.hashedExtLen = int16(len(hashedKey)) - depth
		if hph.trace {
			fmt.Printf("set downHasheKey=[%x]\n", cell.hashedExtension[:cell.hashedExtLen])
		}
	} else {
		if hph.trace {
			fmt.Printf("keep downHasheKey=[%x]\n", cell.hashedExtension[:cell.hashedExtLen])
		}
	}
	if int16(len(plainKey)) == hph.accountKeyLen {
		cell.accountAddrLen = int16(len(plainKey))
		copy(cell.accountAddr[:], plainKey)

		cell.CodeHash = empty.CodeHash
	} else { // set storage key
		cell.storageAddrLen = int16(len(plainKey))
		copy(cell.storageAddr[:], plainKey)
	}
	cell.stateHashLen = 0

	cell.setFromUpdate(u)
	if hph.trace {
		fmt.Printf("updateCell %x => %s\n", plainKey, u.String())
	}
	return cell
}

func (hph *HexPatriciaHashed) RootHash() ([]byte, error) {
	hph.root.stateHashLen = 0
	rootHash, err := hph.computeCellHash(&hph.root, 0, nil)
	if err != nil {
		return nil, err
	}
	return rootHash[1:], nil // first byte is 128+hash_len=160
}

func (hph *HexPatriciaHashed) followAndUpdate(hashedKey, plainKey []byte, stateUpdate *Update) (err error) {
	//if hph.trace {
	// fmt.Printf("mnt: %0x current: %x path %x\n", hph.mountedNib, hph.currentKey[:hph.currentKeyLen], hashedKey)
	//}
	// Keep folding until the currentKey is the prefix of the key we modify
	for hph.needFolding(hashedKey) {
		var foldDone func()
		if dbg.KVReadLevelledMetrics {
			foldDone = hph.metrics.StartFolding(plainKey)
		}
		if err := hph.fold(); err != nil {
			return fmt.Errorf("fold: %w", err)
		}
		if foldDone != nil {
			foldDone()
		}
	}
	// Now unfold until we step on an empty cell
	for unfolding := hph.needUnfolding(hashedKey); unfolding > 0; unfolding = hph.needUnfolding(hashedKey) {
		printLater := hph.currentKeyLen == 0 && hph.mounted && hph.trace
		var unfoldDone func()
		if dbg.KVReadLevelledMetrics {
			unfoldDone = hph.metrics.StartUnfolding(plainKey)
		}
		if err := hph.unfold(hashedKey, unfolding); err != nil {
			return fmt.Errorf("unfold: %w", err)
		}
		if unfoldDone != nil {
			unfoldDone()
		}
		if printLater {
			fmt.Printf("[%x] subtrie pref '%x' d=%d\n", hph.mountedNib, hph.currentKey[:hph.currentKeyLen], hph.depths[max(0, hph.activeRows-1)])
		}
		// fmt.Printf("mnt: %0x current: %x path %x\n", hph.mountedNib, hph.currentKey[:hph.currentKeyLen], hashedKey)
	}

	if stateUpdate == nil {
		// Update the cell
		if int16(len(plainKey)) == hph.accountKeyLen {
			hph.metrics.AccountLoad(plainKey)
			stateUpdate, err = hph.accountFromCacheOrDB(plainKey)
			if err != nil {
				return fmt.Errorf("GetAccount for key %x failed: %w", plainKey, err)
			}
		} else {
			hph.metrics.StorageLoad(plainKey)
			stateUpdate, err = hph.storageFromCacheOrDB(plainKey)
			if err != nil {
				return fmt.Errorf("GetStorage for key %x failed: %w", plainKey, err)
			}
		}
	}
	hph.updateCell(plainKey, hashedKey, stateUpdate)

	mxTrieProcessedKeys.Inc()
	return nil
}

func (hph *HexPatriciaHashed) foldMounted(ctx context.Context, nib int) (cell, error) {
	if nib != hph.mountedNib {
		panic(fmt.Sprintf("foldMounted: nib (%x)!= mountedNib (%x)", nib, hph.mountedNib))
	}

	if hph.trace {
		fmt.Printf("====[%x] folding rows %d depths %+v\n", hph.mountedNib, hph.activeRows, hph.depths[:hph.activeRows])
		defer func() { fmt.Printf("=======[%x] folded =========\n", hph.mountedNib) }()
	}

	for hph.activeRows > 0 {
		if err := ctx.Err(); err != nil {
			return cell{}, err
		}
		// fmt.Printf("===[%x] folding prefix %x (len %d)\n", hph.mountedNib, hph.currentKey[:hph.currentKeyLen], hph.currentKeyLen)
		if hph.activeRows == 1 && hph.depths[hph.activeRows-1] == 1 {
			if hph.trace {
				fmt.Printf("mount early as nibble %02x %s\n", hph.mountedNib, hph.grid[0][hph.mountedNib].String())
			}
			// fmt.Printf("===[%x] stop folding at %x\n", hph.mountedNib, hph.currentKey[:hph.currentKeyLen])
			return hph.grid[0][hph.mountedNib], nil
		}
		if err := hph.fold(); err != nil {
			return cell{}, fmt.Errorf("final fold: %w", err)
		}
	}

	if hph.trace {
		fmt.Printf("===[%x] !@folded to the root\n", hph.mountedNib)
	}
	if hph.rootPresent && hph.rootTouched {
		if hph.trace {
			fmt.Printf("mount root as %02x %s\n", hph.mountedNib, hph.root.String())
		}
		return hph.root, nil
	}
	if hph.trace {
		fmt.Printf("mount as nibble %02x %s\n", hph.mountedNib, hph.grid[0][hph.mountedNib].String())
	}
	// todo potential bug
	return hph.grid[0][hph.mountedNib], nil
}

// ApplyAndClearInlineDeferredUpdates applies deferred updates inline via ctx.PutBranch and clears them.
func (hph *HexPatriciaHashed) ApplyAndClearInlineDeferredUpdates() error {
	if err := hph.branchEncoder.ApplyDeferredUpdates(runtime.NumCPU(), hph.ctx.PutBranch); err != nil {
		return fmt.Errorf("apply deferred updates: %w", err)
	}
	hph.branchEncoder.ClearDeferred()
	return nil
}

// Reset allows HexPatriciaHashed instance to be reused for the new commitment calculation
func (hph *HexPatriciaHashed) Reset() {
	hph.root.reset()
	hph.rootTouched = false
	hph.rootChecked = false
	hph.rootPresent = true
}

func (hph *HexPatriciaHashed) ResetContext(ctx PatriciaContext) {
	hph.ctx = ctx
}

// branchFromCacheOrDB reads branch data from cache if available, otherwise from DB.
func (hph *HexPatriciaHashed) branchFromCacheOrDB(key []byte) ([]byte, error) {
	data, _, err := hph.ctx.Branch(key)
	return data, err
}

// accountFromCacheOrDB reads account data from cache if available, otherwise from DB.
func (hph *HexPatriciaHashed) accountFromCacheOrDB(plainKey []byte) (*Update, error) {
	return hph.ctx.Account(plainKey)
}

// storageFromCacheOrDB reads storage data from cache if available, otherwise from DB.
func (hph *HexPatriciaHashed) storageFromCacheOrDB(plainKey []byte) (*Update, error) {
	return hph.ctx.Storage(plainKey)
}

type stateRootFlag int8

var (
	stateRootPresent stateRootFlag = 1
	stateRootChecked stateRootFlag = 2
	stateRootTouched stateRootFlag = 4
)

// represents state of the tree
type state struct {
	Root         []byte      // encoded root cell
	Depths       [128]int16  // For each row, the depth of cells in that row
	TouchMap     [128]uint16 // For each row, bitmap of cells that were either present before modification, or modified or deleted
	AfterMap     [128]uint16 // For each row, bitmap of cells that were present after modification
	BranchBefore [128]bool   // For each row, whether there was a branch node in the database loaded in unfold
	RootChecked  bool        // Set to false if it is not known whether the root is empty, set to true if it is checked
	RootTouched  bool
	RootPresent  bool
}

func (s *state) Encode(buf []byte) ([]byte, error) {
	var rootFlags stateRootFlag
	if s.RootPresent {
		rootFlags |= stateRootPresent
	}
	if s.RootChecked {
		rootFlags |= stateRootChecked
	}
	if s.RootTouched {
		rootFlags |= stateRootTouched
	}

	ee := bytes.NewBuffer(buf)
	if err := binary.Write(ee, binary.BigEndian, int8(rootFlags)); err != nil {
		return nil, fmt.Errorf("encode rootFlags: %w", err)
	}
	if err := binary.Write(ee, binary.BigEndian, uint16(len(s.Root))); err != nil {
		return nil, fmt.Errorf("encode root len: %w", err)
	}
	if n, err := ee.Write(s.Root); err != nil || n != len(s.Root) {
		return nil, fmt.Errorf("encode root: %w", err)
	}
	d := make([]byte, len(s.Depths))
	for i := 0; i < len(s.Depths); i++ {
		d[i] = byte(s.Depths[i])
	}
	if n, err := ee.Write(d); err != nil || n != len(s.Depths) {
		return nil, fmt.Errorf("encode depths: %w", err)
	}
	if err := binary.Write(ee, binary.BigEndian, s.TouchMap); err != nil {
		return nil, fmt.Errorf("encode touchMap: %w", err)
	}
	if err := binary.Write(ee, binary.BigEndian, s.AfterMap); err != nil {
		return nil, fmt.Errorf("encode afterMap: %w", err)
	}

	var before1, before2 uint64
	for i := 0; i < 64; i++ {
		if s.BranchBefore[i] {
			before1 |= 1 << i
		}
	}
	for i, j := 64, 0; i < 128; i, j = i+1, j+1 {
		if s.BranchBefore[i] {
			before2 |= 1 << j
		}
	}
	if err := binary.Write(ee, binary.BigEndian, before1); err != nil {
		return nil, fmt.Errorf("encode branchBefore_1: %w", err)
	}
	if err := binary.Write(ee, binary.BigEndian, before2); err != nil {
		return nil, fmt.Errorf("encode branchBefore_2: %w", err)
	}
	return ee.Bytes(), nil
}

func (cell *cell) Encode() []byte {
	var pos = int16(1)
	size := pos + 5 + cell.hashLen + cell.accountAddrLen + cell.storageAddrLen + cell.hashedExtLen + cell.extLen // max size
	buf := make([]byte, size)

	var flags uint8
	if cell.hashLen != 0 {
		flags |= cellFlagHash
		buf[pos] = byte(cell.hashLen)
		pos++
		copy(buf[pos:pos+cell.hashLen], cell.hash[:])
		pos += cell.hashLen
	}
	if cell.accountAddrLen != 0 {
		flags |= cellFlagAccount
		buf[pos] = byte(cell.accountAddrLen)
		pos++
		copy(buf[pos:pos+cell.accountAddrLen], cell.accountAddr[:])
		pos += cell.accountAddrLen
	}
	if cell.storageAddrLen != 0 {
		flags |= cellFlagStorage
		buf[pos] = byte(cell.storageAddrLen)
		pos++
		copy(buf[pos:pos+cell.storageAddrLen], cell.storageAddr[:])
		pos += cell.storageAddrLen
	}
	if cell.hashedExtLen != 0 {
		flags |= cellFlagDownHash
		buf[pos] = byte(cell.hashedExtLen)
		pos++
		copy(buf[pos:pos+cell.hashedExtLen], cell.hashedExtension[:cell.hashedExtLen])
		pos += cell.hashedExtLen
	}
	if cell.extLen != 0 {
		flags |= cellFlagExtension
		buf[pos] = byte(cell.extLen)
		pos++
		copy(buf[pos:pos+cell.extLen], cell.extension[:])
		pos += cell.extLen //nolint
	}
	if cell.Deleted() {
		flags |= cellFlagDelete
	}
	buf[0] = flags
	return buf
}

const (
	cellFlagHash = uint8(1 << iota)
	cellFlagAccount
	cellFlagStorage
	cellFlagDownHash
	cellFlagExtension
	cellFlagDelete
)

// Encode current state of hph into bytes
func (hph *HexPatriciaHashed) EncodeCurrentState(buf []byte) ([]byte, error) {
	s := state{
		RootChecked: hph.rootChecked,
		RootTouched: hph.rootTouched,
		RootPresent: hph.rootPresent,
	}
	if hph.currentKeyLen > 0 {
		panic("currentKeyLen > 0")
	}

	s.Root = hph.root.Encode()
	copy(s.Depths[:], hph.depths[:])
	copy(s.BranchBefore[:], hph.branchBefore[:])
	copy(s.TouchMap[:], hph.touchMap[:])
	copy(s.AfterMap[:], hph.afterMap[:])

	return s.Encode(buf)
}
