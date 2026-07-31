//go:build cgo_reth

package reth

// BytecodeWriter writes contract bytecodes to the Bytecodes MDBX table,
// deduplicating by keccak256(code) via a bounded LRU cache.
//
// Wire format (reth Compact, LegacyAnalyzed variant):
//
//	[u32 BE: analyzed_len]
//	[analyzed_bytes]        — original code + STOP-padding
//	[u8: 0x02]              — LEGACY_ANALYZED_BYTECODE_ID
//	[u64 BE: original_len]  — unpadded length
//	[jump_table_bytes]      — ceil(analyzed_len/8) bytes, one bit per opcode
//	                          position, 1 = JUMPDEST; LSB-first within each byte
//	                          (bitvec<u8, Lsb0> layout)
//
// Wire format (reth Compact, Eip7702 variant) — delegation designators only:
//
//	[u32 BE: 23]
//	[raw_bytes]             — 0xEF 0x01 0x00 || 20-byte delegate address
//	[u8: 0x04]              — EIP7702_BYTECODE_ID; no original_len, no jump table
//
// Reth uses LegacyAnalyzed for all regular contracts. The LEGACY_RAW (0x00)
// variant appears in from_compact for backward compat but is never written by
// new_raw (which always runs analyze_legacy and writes LegacyAnalyzed).
// We match that behaviour exactly.
//
// A designator MUST NOT be written as LegacyAnalyzed. reth's from_compact
// rebuilds that variant through new_analyzed, which never inspects the 0xEF01
// prefix, so the account ends up holding the 23 designator bytes as ordinary
// executable code: calling it dies on the undefined 0xEF opcode
// (OpcodeNotFound, all gas consumed) instead of dispatching to the delegate.
// Only EIP7702_BYTECODE_ID routes from_compact through new_raw, which
// classifies by prefix. The bug is invisible to the state root — the trie
// commits keccak(code), not the variant tag — and geth is unaffected because
// it stores raw code and parses the prefix at call time.

import (
	"encoding/binary"
	"errors"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// bytecodeAnalyzed is the discriminant for the LegacyAnalyzed variant.
// Matches compact_ids::LEGACY_ANALYZED_BYTECODE_ID = 2 in reth-primitives-traits.
const bytecodeAnalyzed uint8 = 2

// bytecodeEip7702 is the discriminant for the Eip7702 variant.
// Matches compact_ids::EIP7702_BYTECODE_ID = 4 in reth-primitives-traits.
const bytecodeEip7702 uint8 = 4

// eip7702DesignatorLen is revm's EIP7702_BYTECODE_LEN: 2 (magic) + 1 (version)
// + 20 (address). revm accepts a designator only at exactly this length and
// version; anything else that merely starts with 0xEF01 is an
// Eip7702DecodeError there, so it stays legacy code here (matching geth, whose
// ParseDelegation is equally strict).
const eip7702DesignatorLen = 23

// eip7702DesignatorPrefix is the 3-byte EIP-7702 header — revm's
// EIP7702_MAGIC_BYTES (0xEF01) followed by EIP7702_VERSION (0).
var eip7702DesignatorPrefix = [3]byte{0xEF, 0x01, 0x00}

// opcode constants used by the bytecode analysis pass.
const (
	opJUMPDEST = uint8(0x5B)
	opPUSH1    = uint8(0x60)
	opPUSH32   = uint8(0x7F)
	opDUPN     = uint8(0xE6)
	opSWAPN    = uint8(0xE7)
	opEXCHANGE = uint8(0xE8)
	opSTOP     = uint8(0x00)
)

// BytecodeWriter writes contract bytecodes deduped by keccak256(code).
//
// Dedup strategy: bounded LRU + DB seek on LRU miss. The LRU avoids the
// unbounded HashSet pattern that reth init.rs uses (~600 MB at 15M unique
// contracts). On LRU miss, a single mdbx.Get checks the DB before
// writing — adds at most O(log N) IOPS per miss.
type BytecodeWriter struct {
	txn  *mdbx.Txn
	dbi  mdbx.DBI
	seen *lru.Cache[common.Hash, struct{}]
}

// NewBytecodeWriter constructs a writer with a bounded-size LRU.
// capacity is a hint; recommend ~100_000 for ~4 MB RAM bound.
func NewBytecodeWriter(txn *mdbx.Txn, dbi mdbx.DBI, capacity int) *BytecodeWriter {
	cache, err := lru.New[common.Hash, struct{}](capacity)
	if err != nil {
		panic(fmt.Sprintf("BytecodeWriter: lru.New failed: %v", err))
	}
	return &BytecodeWriter{txn: txn, dbi: dbi, seen: cache}
}

// Write the bytecode under its keccak-256 hash. Returns the hash for splice
// into Account.BytecodeHash, and a bool reporting whether the code was
// actually written this call (false on LRU hit, DB hit, or empty code).
// Callers that bill bytes-written stats should gate the increment on the
// returned bool so dedup hits don't inflate the count.
//
// Empty bytecode is allowed (returns the canonical KECCAK_EMPTY hash) but
// is NOT written to the DB; reth treats EmptyCodeHash specially.
func (w *BytecodeWriter) Write(code []byte) (common.Hash, bool, error) {
	hash := crypto.Keccak256Hash(code)

	// Empty code: don't store; reth handles EmptyCodeHash specially.
	if len(code) == 0 {
		return hash, false, nil
	}

	// Already seen via LRU?
	if _, ok := w.seen.Get(hash); ok {
		return hash, false, nil
	}

	// LRU miss: check DB to avoid duplicate writes.
	if _, err := w.txn.Get(w.dbi, hash[:]); err == nil {
		// Already in DB; update LRU and return.
		w.seen.Add(hash, struct{}{})
		return hash, false, nil
	} else if !errors.Is(err, mdbx.ErrNotFound) {
		return common.Hash{}, false, fmt.Errorf("BytecodeWriter.Write: get %s: %w", hash.Hex(), err)
	}

	// Encode and write.
	val := encodeBytecodeCompact(code)
	if err := w.txn.Put(w.dbi, hash[:], val, 0); err != nil {
		return common.Hash{}, false, fmt.Errorf("BytecodeWriter.Write: put %s: %w", hash.Hex(), err)
	}
	w.seen.Add(hash, struct{}{})
	return hash, true, nil
}

// encodeBytecodeCompact encodes code into reth's Bytecode Compact wire format.
//
// Mirrors revm_bytecode::Bytecode::to_compact exactly:
//
//  1. EIP-7702 delegation designators take the Eip7702 variant (see
//     encodeEip7702Compact) — reth's to_compact branches on is_legacy(), and
//     only that variant survives the from_compact round-trip as a delegation.
//  2. Everything else: analyze the bytecode (find JUMPDESTs, compute padding)
//     — same logic as revm_bytecode::legacy::analyze_legacy() — then write:
//     [u32 BE analyzed_len][analyzed_bytes][u8=2][u64 BE original_len][jump_table]
//
// jump_table is ceil(analyzed_len/8) bytes; bit i (LSB-first within each byte)
// is set iff the opcode at position i is JUMPDEST. This is bitvec<u8, Lsb0>
// layout.
func encodeBytecodeCompact(code []byte) []byte {
	if isEip7702Designator(code) {
		return encodeEip7702Compact(code)
	}

	originalLen := len(code)

	// --- analysis pass: mirror analyze_legacy ---
	// jumps[i] = true iff code[i] is JUMPDEST.
	jumps := make([]bool, originalLen)

	i := 0
	var lastByte uint8
	var prevByte uint8
	for i < originalLen {
		prevByte = lastByte
		lastByte = code[i]
		if lastByte == opJUMPDEST {
			jumps[i] = true
			i++
		} else {
			pushOffset := int(lastByte) - int(opPUSH1)
			if pushOffset >= 0 && pushOffset < 32 {
				// PUSH1..PUSH32: skip immediate bytes
				i += pushOffset + 2
			} else {
				i++
			}
		}
	}

	// push_overflow: how many bytes we overshot past end
	pushOverflow := i - originalLen
	padding := pushOverflow

	if lastByte == opSTOP {
		// If last opcode is STOP, check if prev was DUPN/SWAPN/EXCHANGE
		// (they have 1-byte immediates not handled above)
		if isDupnSwapnExchange(prevByte) {
			padding++
		}
	} else {
		// Add a STOP + possibly extra byte for DUPN/SWAPN/EXCHANGE
		padding++
		if isDupnSwapnExchange(lastByte) {
			padding++
		}
	}

	// Special case: empty bytecode is handled by the caller (not reached here).
	// But if code is non-empty and padding=0 after analysis (last byte was STOP
	// and prev was not DUPN/SWAPN/EXCHANGE), analyzedLen == originalLen.
	analyzedLen := originalLen + padding

	// Build analyzed bytes (original + zero-pad)
	analyzed := make([]byte, analyzedLen)
	copy(analyzed, code)
	// Remaining bytes are already zero (padding with STOP=0x00).

	// Build jump table: ceil(originalLen/8) bytes, bitvec<u8, Lsb0>.
	// reth/revm initialize the bitvec at original length: `bitvec![u8, Lsb0; 0;
	// bytecode.len()]` — the bitmap is NOT extended during padding analysis.
	// Bit at position pos: byte = pos/8, bit offset = pos%8 (0=LSB).
	jtBytes := (originalLen + 7) / 8
	jumpTable := make([]byte, jtBytes)
	for pos, isJump := range jumps {
		if !isJump || pos >= originalLen {
			// JUMPDESTs in padding bytes don't exist (padding is all-zero STOP)
			// but guard out-of-range writes anyway.
			continue
		}
		byteIdx := pos / 8
		bitIdx := uint(pos % 8)
		jumpTable[byteIdx] |= 1 << bitIdx
	}

	// Assemble wire format:
	// [u32 BE analyzed_len][analyzed_bytes][u8 LEGACY_ANALYZED=2][u64 BE original_len][jump_table]
	buf := make([]byte, 4+analyzedLen+1+8+jtBytes)
	off := 0

	binary.BigEndian.PutUint32(buf[off:], uint32(analyzedLen))
	off += 4

	copy(buf[off:], analyzed)
	off += analyzedLen

	buf[off] = bytecodeAnalyzed
	off++

	binary.BigEndian.PutUint64(buf[off:], uint64(originalLen))
	off += 8

	copy(buf[off:], jumpTable)

	return buf
}

// isDupnSwapnExchange returns true for DUPN (0xE6), SWAPN (0xE7), EXCHANGE (0xE8).
// These opcodes have 1-byte immediates not tracked by the PUSH analysis pass.
func isDupnSwapnExchange(op uint8) bool {
	return op == opDUPN || op == opSWAPN || op == opEXCHANGE
}

// isEip7702Designator reports whether code is a well-formed EIP-7702
// delegation designator, using revm's acceptance rule from
// Bytecode::new_eip7702_raw: exactly 23 bytes, 0xEF01 magic, version byte 0.
// Anything looser (wrong length, unsupported version) is an
// Eip7702DecodeError in revm, so it must keep the legacy encoding.
func isEip7702Designator(code []byte) bool {
	return len(code) == eip7702DesignatorLen &&
		code[0] == eip7702DesignatorPrefix[0] &&
		code[1] == eip7702DesignatorPrefix[1] &&
		code[2] == eip7702DesignatorPrefix[2]
}

// encodeEip7702Compact encodes a delegation designator the way reth's
// to_compact does for the Eip7702 variant: the raw bytes verbatim (revm keeps
// designators unpadded, original_len == 23) followed by the discriminant.
// from_compact hands these bytes to new_raw, which re-derives the delegation
// from the 0xEF01 prefix — no original_len or jump table is stored.
//
//	[u32 BE: 23][23 raw bytes][u8: 0x04]
func encodeEip7702Compact(code []byte) []byte {
	buf := make([]byte, 4+len(code)+1)
	binary.BigEndian.PutUint32(buf, uint32(len(code)))
	copy(buf[4:], code)
	buf[4+len(code)] = bytecodeEip7702
	return buf
}
