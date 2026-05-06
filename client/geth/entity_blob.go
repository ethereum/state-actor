package geth

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// entityKind tags the Phase-1 scratch blob so Phase 2 can decode without
// knowing the kind a priori. EOA values omit code/storage; contract
// values carry both. Genesis-alloc accounts encode as contracts when
// they have alloc storage or code, EOAs otherwise.
type entityKind byte

const (
	entityEOA      entityKind = 1
	entityContract entityKind = 2
)

// entityBlob is the in-memory representation of a Phase-1 entity after
// decoding. Phase 2 builds storage tries from the slot list and writes
// flat-state from these fields.
type entityBlob struct {
	kind    entityKind
	nonce   uint64
	balance *uint256.Int
	code    []byte
	// slots is intentionally a flat slice (not a map) so encode/decode
	// preserves the input ordering. Phase 2 re-sorts by keccak(key) for
	// MPT trie construction.
	slots []entityBlobSlot
}

// entityBlobSlot pairs a raw 32-byte storage key with its 32-byte value.
type entityBlobSlot struct {
	Key   common.Hash
	Value common.Hash
}

// Wire format (per blob, single-byte kind tag + fields):
//
//	EOA:
//	  [0x01] [nonce u64 BE] [balance_len u8] [balance bytes...]
//
//	Contract:
//	  [0x02] [nonce u64 BE] [balance_len u8] [balance bytes...]
//	     [code_len u32 BE] [code bytes...]
//	     [slot_count u32 BE] [slot_count × ([slot_key 32B] [slot_value 32B])]
//
// Format mirrors client/besu/state_writer_cgo.go's encodeEntityEOA /
// encodeEntityContract. The Phase-1 scratch DB is internal — wire-format
// changes only need golden-hash-test regen, not cross-client coordination.

func encodeEntityEOA(nonce uint64, balance *uint256.Int) []byte {
	balBytes := balance.ToBig().Bytes()
	out := make([]byte, 1+8+1+len(balBytes))
	out[0] = byte(entityEOA)
	binary.BigEndian.PutUint64(out[1:9], nonce)
	out[9] = byte(len(balBytes))
	copy(out[10:], balBytes)
	return out
}

func encodeEntityContract(nonce uint64, balance *uint256.Int, code []byte, slots []entityBlobSlot) []byte {
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
	for _, s := range slots {
		out = append(out, s.Key[:]...)
		out = append(out, s.Value[:]...)
	}
	return out
}

func decodeEntityBlob(blob []byte) (entityBlob, error) {
	var e entityBlob
	if len(blob) < 1 {
		return e, fmt.Errorf("entity blob too short: %d bytes", len(blob))
	}
	e.kind = entityKind(blob[0])
	if e.kind != entityEOA && e.kind != entityContract {
		return e, fmt.Errorf("entity blob unknown kind %d", e.kind)
	}

	pos := 1
	if len(blob) < pos+8+1 {
		return e, fmt.Errorf("entity blob truncated at nonce/balance header")
	}
	e.nonce = binary.BigEndian.Uint64(blob[pos : pos+8])
	pos += 8
	balLen := int(blob[pos])
	pos++
	if len(blob) < pos+balLen {
		return e, fmt.Errorf("entity blob truncated at balance bytes")
	}
	e.balance = new(uint256.Int)
	e.balance.SetBytes(blob[pos : pos+balLen])
	pos += balLen

	if e.kind == entityEOA {
		if pos != len(blob) {
			return e, fmt.Errorf("entity EOA blob has trailing %d bytes", len(blob)-pos)
		}
		return e, nil
	}

	// Contract: code + slots.
	if len(blob) < pos+4 {
		return e, fmt.Errorf("entity blob truncated at code length")
	}
	codeLen := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
	pos += 4
	if len(blob) < pos+codeLen {
		return e, fmt.Errorf("entity blob truncated at code bytes")
	}
	e.code = make([]byte, codeLen)
	copy(e.code, blob[pos:pos+codeLen])
	pos += codeLen

	if len(blob) < pos+4 {
		return e, fmt.Errorf("entity blob truncated at slot count")
	}
	slotCount := int(binary.BigEndian.Uint32(blob[pos : pos+4]))
	pos += 4
	if len(blob) < pos+slotCount*64 {
		return e, fmt.Errorf("entity blob truncated at slots (%d)", slotCount)
	}
	e.slots = make([]entityBlobSlot, slotCount)
	for i := 0; i < slotCount; i++ {
		copy(e.slots[i].Key[:], blob[pos:pos+32])
		copy(e.slots[i].Value[:], blob[pos+32:pos+64])
		pos += 64
	}
	if pos != len(blob) {
		return e, fmt.Errorf("entity contract blob has trailing %d bytes", len(blob)-pos)
	}
	return e, nil
}
