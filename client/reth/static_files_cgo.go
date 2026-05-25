//go:build cgo_reth

package reth

// WriteStaticFiles writes block-0 segment files under <datadir>/static_files/.
//
// Reth keeps canonical headers, transactions, receipts, and transaction senders
// in nippy-jar static-file segments. These files are required for reth node
// --dev to boot — without them, the node fails with "missing static file block 0".
//
// Format mirrors crates/storage/nippy-jar/src/ at PinnedRethCommit:
//
//   - Each segment produces three files in <datadir>/static_files/:
//       static_file_{segment}_0_499999          — LZ4-compressed column data (NO extension)
//       static_file_{segment}_0_499999.off      — offset table (1+n*8 bytes)
//       static_file_{segment}_0_499999.conf     — bincode NippyJar<SegmentHeader>
//   - Change-based segments (account/storage changesets) additionally produce:
//       static_file_{segment}_0_499999.csoff    — 16-byte changeset offset records
//
//   - Data file: each column value is independently LZ4-compressed (raw block format,
//     matching lz4_flex::block::compress — no size prefix). Columns are concatenated.
//     The .off file contains byte offsets into this concatenated compressed data.
//
//   - Bincode encoding (little-endian, no padding, length-prefixed strings):
//       NippyJar: version(u64) + user_header + columns(u64) + rows(u64) +
//                 compressor(Option<Lz4>) + max_row_size(u64)
//       SegmentHeader: expected_block_range(16) + Option<block_range>(1+16) +
//                      Option<tx_range>(1+16) + segment(u32) [+ csoff_len if change-based]
//       compressor = Some(Lz4): [0x01] + u32_LE(1) = 5 bytes total
//
//   - Offsets file: [offset_size_byte=8] + (rows*columns+1) u64 LE offsets into
//     compressed data. Last offset = expected data file length.
//     For rows=0: [8, 0,0,0,0,0,0,0,0] (9 bytes).
//
//   - max_row_size in conf = sum of UNCOMPRESSED column sizes (reth uses it for
//     decompression buffer sizing).
//
//   - Header data encoding: reth's Compact codec (bitfield + compact fields).
//     See headerCompactBytes for a detailed layout derivation.
//
// Segments written:
//   - headers:              3 columns, 1 row  (genesis header)
//   - transactions:         1 column,  0 rows
//   - receipts:             1 column,  0 rows
//   - transaction-senders:  1 column,  0 rows
//   - account-change-sets:  1 column,  1 row  (empty bootstrap shell + .csoff)
//   - storage-change-sets:  1 column,  1 row  (empty bootstrap shell + .csoff)
//
// The change-based shells are required for reth's persistence service to accept
// block-1 appends; without them reth crashes at the first block with
// UnexpectedStaticFileBlockNumber(AccountChangeSets, 1, 0).
//
// file naming: DEFAULT_BLOCKS_PER_STATIC_FILE = 500_000 so block 0 lives in
// the range 0..=499999. The filename contains the segment string (kebab-case).

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/core/types"
	lz4 "github.com/pierrec/lz4/v4"
)

const (
	// staticFilesDir is the subdirectory reth expects for static files.
	staticFilesDir = "static_files"

	// blocksPerStaticFile mirrors DEFAULT_BLOCKS_PER_STATIC_FILE in reth.
	blocksPerStaticFile = 500_000

	// blockRangeEnd = 0 + 500_000 - 1 = 499_999
	blockRangeEnd = blocksPerStaticFile - 1

	// nippy jar version
	nippyJarVersion = 1
)

// staticFileSegment mirrors reth's StaticFileSegment enum variant indices
// (crates/static-file/types/src/segment.rs, enum order = discriminant).
//
// Headers=0, Transactions=1, Receipts=2, TransactionSenders=3,
// AccountChangeSets=4, StorageChangeSets=5.
type staticFileSegment struct {
	// as_str is the kebab-case directory/filename component.
	name string
	// enumIdx is the u32 bincode discriminant (0-based enum order).
	enumIdx uint32
	// columns is the number of data columns per row.
	columns uint64
}

var (
	segHeaders           = staticFileSegment{"headers", 0, 3}
	segTransactions      = staticFileSegment{"transactions", 1, 1}
	segReceipts          = staticFileSegment{"receipts", 2, 1}
	segTxSenders         = staticFileSegment{"transaction-senders", 3, 1}
	segAccountChangeSets = staticFileSegment{"account-change-sets", 4, 1}
	segStorageChangeSets = staticFileSegment{"storage-change-sets", 5, 1}
)

// isChangeBased mirrors reth's StaticFileSegment::is_change_based
// (crates/static-file/types/src/segment.rs). Change-based segments
// (AccountChangeSets, StorageChangeSets) carry a 5th SegmentHeader field
// (changeset_offsets_len) and a .csoff sidecar file.
func (s staticFileSegment) isChangeBased() bool {
	return s.enumIdx == 4 || s.enumIdx == 5
}

// WriteStaticFiles writes block-0 segments under <datadir>/static_files/.
//
// The headers segment contains the genesis header (block 0); all other
// segments are written as empty (block_range=Some(0..=0), rows=0) so that
// reth's consistency checker finds them at the expected height.
func WriteStaticFiles(datadir string, header *types.Header) error {
	sfDir := filepath.Join(datadir, staticFilesDir)
	if err := os.MkdirAll(sfDir, 0o755); err != nil {
		return fmt.Errorf("WriteStaticFiles: mkdir %s: %w", sfDir, err)
	}

	// Encode the genesis header using reth's Compact codec.
	headerData, err := headerCompactBytes(header)
	if err != nil {
		return fmt.Errorf("WriteStaticFiles: encode header: %w", err)
	}

	// 1. Headers segment — 1 row (the genesis header), 3 columns.
	//    Col 0: Header compact, Col 1: CompactU256(td=0), Col 2: B256(hash).
	col0 := headerData
	col1 := tdCompactBytes()      // total_difficulty = 0
	col2 := header.Hash().Bytes() // B256 block hash

	if err := writeSegment(sfDir, segHeaders, header, [][]byte{col0, col1, col2}); err != nil {
		return fmt.Errorf("WriteStaticFiles: headers: %w", err)
	}

	// 2–4. Empty tx/receipt/senders segments — 0 rows.
	for _, seg := range []staticFileSegment{segTransactions, segReceipts, segTxSenders} {
		if err := writeSegment(sfDir, seg, header, nil); err != nil {
			return fmt.Errorf("WriteStaticFiles: %s: %w", seg.name, err)
		}
	}

	// 5–6. Empty AccountChangeSets / StorageChangeSets bootstrap shells (1 row,
	// empty content + .csoff). Reth's persistence service expects these
	// segments at block_range=Some(0..=0) so block-1 appends pass
	// writer.rs's check_next_block_number. Not history data — a
	// minimum-viable shell, correct for both archive and full-node modes.
	for _, seg := range []staticFileSegment{segAccountChangeSets, segStorageChangeSets} {
		if err := writeSegment(sfDir, seg, header, [][]byte{nil}); err != nil {
			return fmt.Errorf("WriteStaticFiles: %s: %w", seg.name, err)
		}
	}

	return nil
}

// writeSegment produces the three nippy-jar files for one segment.
//
// rows: if colData is nil → 0 rows (empty segment); else 1 row with
// len(colData) columns. colData[i] is the pre-encoded (uncompressed) bytes for column i.
//
// For non-empty segments each column is LZ4-compressed (raw block format) before
// being written to the data file. The .off file references compressed byte offsets.
// max_row_size in the conf records the uncompressed total (for reth's buffer sizing).
func writeSegment(dir string, seg staticFileSegment, header *types.Header, colData [][]byte) error {
	base := filepath.Join(dir, fmt.Sprintf("static_file_%s_0_%d", seg.name, blockRangeEnd))

	rows := uint64(0)
	if colData != nil {
		rows = 1
	}

	// ---- compress each column independently (LZ4 raw block, no size prefix) ----
	var compressedCols [][]byte
	var uncompressedSize uint64
	if colData != nil {
		compressedCols = make([][]byte, len(colData))
		for i, col := range colData {
			uncompressedSize += uint64(len(col))
			compressed, err := lz4CompressBlock(col)
			if err != nil {
				return fmt.Errorf("lz4 compress col %d: %w", i, err)
			}
			compressedCols[i] = compressed
		}
	}

	// ---- data file (NO extension): concatenated compressed columns ----
	var sfData []byte
	if compressedCols != nil {
		var total int
		for _, col := range compressedCols {
			total += len(col)
		}
		sfData = make([]byte, 0, total)
		for _, col := range compressedCols {
			sfData = append(sfData, col...)
		}
	}

	// Reth expects the data file with NO extension (just the base name),
	// e.g. "static_file_headers_0_499999". The .off and .conf files retain
	// their extensions.
	if err := os.WriteFile(base, sfData, 0o644); err != nil {
		return fmt.Errorf("write data file: %w", err)
	}

	// ---- .off offsets file: offsets into compressed data ----
	offBytes := buildOffsetsFile(seg.columns, compressedCols)
	if err := os.WriteFile(base+".off", offBytes, 0o644); err != nil {
		return fmt.Errorf("write .off: %w", err)
	}

	// ---- .conf configuration file ---------------------------
	// changeset_offsets_len: 1 record (block 0 = empty changeset) for the change-based
	// bootstrap shell; 0 / unused for non-change-based segments.
	var changesetOffsetsLen uint64
	if seg.isChangeBased() {
		changesetOffsetsLen = 1
	}
	confBytes, err := buildConfFile(seg, header, rows, uncompressedSize, changesetOffsetsLen)
	if err != nil {
		return fmt.Errorf("build .conf: %w", err)
	}
	if err := os.WriteFile(base+".conf", confBytes, 0o644); err != nil {
		return fmt.Errorf("write .conf: %w", err)
	}

	// ---- .csoff sidecar (change-based segments only) --------
	// Fixed-width 16-byte records: [offset u64 LE | num_changes u64 LE]. For the
	// block-0 bootstrap shell we write a single record of zeros: the changeset
	// starts at offset 0 in the data file and contains 0 changes. Mirrors
	// reth crates/static-file/types/src/changeset_offsets.rs (RECORD_SIZE=16).
	if seg.isChangeBased() {
		csoffBytes := make([]byte, 16) // single 16-byte zero record
		if err := os.WriteFile(base+".csoff", csoffBytes, 0o644); err != nil {
			return fmt.Errorf("write .csoff: %w", err)
		}
	}

	return nil
}

// buildOffsetsFile creates the nippy-jar offsets file bytes.
//
// Layout (from crates/storage/nippy-jar/src/writer.rs):
//
//	byte 0       : offset_size = 8
//	bytes 1..N   : offsets, each 8 bytes LE
//
// For rows=0: just [8, 0,0,0,0,0,0,0,0] (9 bytes, final offset = data size = 0).
// For rows=1, columns=3: [8, off0, off1, off2, off_end] where off_end = total data length.
//
// offsets[i] = byte position in the data file where column i of the single row begins.
// The final extra offset = total data file size (expected by consistency checker).
func buildOffsetsFile(columns uint64, colData [][]byte) []byte {
	if colData == nil {
		// rows=0: just offset_size byte + one 0 offset (expected data file size = 0)
		out := make([]byte, 1+8)
		out[0] = 8
		return out
	}

	// rows=1: offset_size + columns offsets + 1 final offset
	numOffsets := uint64(len(colData)) + 1
	out := make([]byte, 1+numOffsets*8)
	out[0] = 8

	var pos uint64
	for i, col := range colData {
		binary.LittleEndian.PutUint64(out[1+i*8:], pos)
		pos += uint64(len(col))
	}
	// Final offset = total data size
	binary.LittleEndian.PutUint64(out[1+len(colData)*8:], pos)

	return out
}

// buildConfFile serializes NippyJar<SegmentHeader> in bincode format.
//
// Bincode encoding rules (confirmed by test snapshots in
// crates/static-file/types/src/snapshots/):
//
//	struct serialized field-by-field in definition order
//	u64/usize: 8 bytes LE
//	u32 (enum discriminant): 4 bytes LE
//	Option: 1 byte (0=None, 1=Some) + inner if Some
//	unit type (): 0 bytes
//	#[serde(skip)] fields: not serialized
//
// NippyJar field order: version, user_header, columns, rows, compressor, max_row_size
// (#[serde(skip)] filter and phf are absent).
//
// SegmentHeader field order: expected_block_range, block_range, tx_range, segment
// (changeset_offsets_len only for is_change_based() segments — not written here).
//
// uncompressedSize is the sum of uncompressed column byte sizes for the one row
// (0 for empty segments). It is written as max_row_size so reth allocates a
// large-enough decompression buffer.
//
// For non-empty segments: compressor = Some(Lz4) → [0x01, 0x01, 0x00, 0x00, 0x00].
// For empty segments (rows=0): compressor = None → [0x00].
func buildConfFile(seg staticFileSegment, header *types.Header, rows uint64, uncompressedSize uint64, changesetOffsetsLen uint64) ([]byte, error) {
	// --- SegmentHeader ---
	// For headers: block_range=Some(0..=0), tx_range=None
	// For tx/receipt/senders: block_range=Some(0..=0), tx_range=Some(0..=0)
	//   (even with 0 rows, block_range=Some tells reth the segment covers block 0)
	// For change-based (account/storage changesets): block_range=Some(0..=0),
	//   tx_range=None, plus a 5th changeset_offsets_len field.
	userHeaderBytes := buildSegmentHeaderBytes(seg, changesetOffsetsLen)

	// --- NippyJar ---
	out := make([]byte, 0, 85+len(userHeaderBytes))
	out = appendLE64(out, nippyJarVersion)
	out = append(out, userHeaderBytes...)
	out = appendLE64(out, seg.columns)
	out = appendLE64(out, rows)

	// compressor field: Option<Compressors>
	// - Empty segments (rows=0): None → [0x00]
	// - Non-empty segments: Some(Lz4) → [0x01, 0x01, 0x00, 0x00, 0x00]
	//   (0x01 = Some, u32 LE = 1 = Lz4 enum variant index, Lz4 struct has no fields)
	if rows > 0 {
		out = append(out, 0x01)  // Option: Some
		out = appendLE32(out, 1) // Compressors::Lz4 discriminant (variant index 1)
		// Lz4 struct {} has no fields → 0 more bytes
	} else {
		out = append(out, 0x00) // Option: None
	}

	// max_row_size: uncompressed byte count (reth uses this to size decompression buffer)
	out = appendLE64(out, uncompressedSize)

	return out, nil
}

// buildSegmentHeaderBytes serializes a SegmentHeader in bincode.
//
// All genesis segments use block_range=Some(0..=0) so that iter_static_files
// finds them. Tx-based segments (transactions, receipts, senders) additionally
// use tx_range=Some(0..=0). Change-based segments (account/storage changesets)
// have tx_range=None AND a 5th changeset_offsets_len:u64 field.
//
// The expected_block_range spans the full file slot: 0..=499_999.
//
// changesetOffsetsLen is the record count in the matching .csoff sidecar;
// for non-change-based segments it must be 0 and is not serialized.
func buildSegmentHeaderBytes(seg staticFileSegment, changesetOffsetsLen uint64) []byte {
	out := make([]byte, 0, 64)

	// expected_block_range: 0..=499_999
	out = appendLE64(out, 0)
	out = appendLE64(out, blockRangeEnd)

	// block_range: Some(0..=0) for all segments
	out = append(out, 0x01)  // Some
	out = appendLE64(out, 0) // start
	out = appendLE64(out, 0) // end

	// tx_range: None for block-based (Headers) and change-based (AccountChangeSets,
	// StorageChangeSets) per StaticFileSegment::is_tx_based. Some(0..=0) for the
	// three tx-based segments (Transactions, Receipts, TransactionSenders).
	switch seg.enumIdx {
	case 0, 4, 5: // Headers, AccountChangeSets, StorageChangeSets — no tx range
		out = append(out, 0x00) // None
	default: // Transactions, Receipts, TransactionSenders — tx-based
		out = append(out, 0x01) // Some
		out = appendLE64(out, 0)
		out = appendLE64(out, 0)
	}

	// segment enum discriminant (u32 LE)
	out = appendLE32(out, seg.enumIdx)

	// changeset_offsets_len: 5th field, only for change-based segments. Reth's
	// SegmentHeader Serialize impl (crates/static-file/types/src/segment.rs)
	// uses len=5 when segment.is_change_based() and serialize_field("changeset_offsets", ...).
	if seg.isChangeBased() {
		out = appendLE64(out, changesetOffsetsLen)
	}

	return out
}

// headerCompactBytes encodes a go-ethereum types.Header into reth's Compact
// binary format.
//
// The Compact codec for alloy_consensus::Header is derived via the #[derive(Compact)]
// macro (reth-codecs-0.3.1, crates/storage/nippy-jar/../alloy/header.rs). Layout:
//
//  1. Bitflag header (4 bytes):
//     bit  0        : withdrawals_root (Option, 1 bit)
//     bits 1–6      : difficulty_len   (U256,   6 bits)
//     bits 7–10     : number_len       (u64,    4 bits)
//     bits 11–14    : gas_limit_len    (u64,    4 bits)
//     bits 15–18    : gas_used_len     (u64,    4 bits)
//     bits 19–22    : timestamp_len    (u64,    4 bits)
//     bits 23–26    : nonce_len        (u64,    4 bits)
//     bit  27       : base_fee_per_gas (Option, 1 bit)
//     bit  28       : blob_gas_used    (Option, 1 bit)
//     bit  29       : excess_blob_gas  (Option, 1 bit)
//     bit  30       : parent_beacon_block_root (Option, 1 bit)
//     bit  31       : extra_fields     (Option, 1 bit)
//     total = 32 bits = 4 bytes ✓ (matches Header::bitflag_encoded_bytes() == 4)
//
//  2. Verbatim fields (written in struct order, full size):
//     parent_hash (B256, 32)
//     ommers_hash (B256, 32)
//     beneficiary (Address, 20)
//     state_root (B256, 32)
//     transactions_root (B256, 32)
//     receipts_root (B256, 32)
//
//  3. withdrawals_root (Option<B256>): uses specialized_to_compact, writes B256 raw (32 bytes) if Some.
//
//  4. logs_bloom (Bloom, 256 bytes verbatim).
//
//  5. Compact fields (written in struct order, min-bytes BE, length from bitfield):
//     difficulty (U256, len from bitfield bits 1–6)
//     number     (u64,  len from bitfield bits 7–10)
//     gas_limit  (u64,  len from bitfield bits 11–14)
//     gas_used   (u64,  len from bitfield bits 15–18)
//     timestamp  (u64,  len from bitfield bits 19–22)
//
//  6. mix_hash (B256, 32 bytes verbatim).
//
//  7. nonce (u64, len from bitfield bits 23–26).
//
//  8. base_fee_per_gas (Option<u64>): varuint(len) + BE bytes of value (if Some).
//
//  9. blob_gas_used (Option<u64>): varuint(len) + BE bytes.
//
// 10. excess_blob_gas (Option<u64>): varuint(len) + BE bytes.
//
// 11. parent_beacon_block_root (Option<B256>): specialized_to_compact, writes B256 raw if Some.
//
//  12. extra_fields (Option<HeaderExt>): when Some (Prague-active genesis).
//     Uses the NON-specialized Option<T> form (reth-codecs-0.3.1
//     lib.rs): `varuint(N) || HeaderExt_bytes`. HeaderExt is itself
//     Compact-derived with three optional fields (requests_hash,
//     block_access_list_hash, slot_number) prefixed by a 1-byte inner
//     bitflag (bits 0/1/2 = presence). For Prague-active genesis only
//     RequestsHash is set, so inner = 0x01 || 32-byte B256 (33 bytes),
//     varuint(33) = 0x21 → 34 bytes appended.
//
// 13. extra_data (Bytes): written verbatim (last field, length = buf.len() - consumed).
//
// Verification: Holesky block 1947953 test vector in reth-codecs-0.3.1/src/alloy/header.rs
// confirms this exact layout.
func headerCompactBytes(h *types.Header) ([]byte, error) {
	// Compute compact lengths for numeric fields.
	diffLen := u256CompactLen(h.Difficulty)
	numberLen := u64CompactLen(h.Number.Uint64())
	gasLimitLen := u64CompactLen(h.GasLimit)
	gasUsedLen := u64CompactLen(h.GasUsed)
	timestampLen := u64CompactLen(h.Time)
	nonceLen := u64CompactLen(h.Nonce.Uint64())

	// Presence bits for optional fields.
	hasWithdrawals := h.WithdrawalsHash != nil
	hasBaseFee := h.BaseFee != nil
	hasBlobGasUsed := h.BlobGasUsed != nil
	hasExcessBlobGas := h.ExcessBlobGas != nil
	hasParentBeaconRoot := h.ParentBeaconRoot != nil
	// extra_fields (Option<HeaderExt>) is Some iff RequestsHash is set —
	// which genesisheader.Build does iff Prague is active. For a Prague-
	// active genesis the inner HeaderExt.requests_hash is also Some
	// (set to EmptyRequestsHash).
	hasExtraFields := h.RequestsHash != nil

	// Build the 4-byte bitfield LSB-first (matches modular_bitfield LSB packing).
	var bits uint32
	bitPos := 0
	packBits := func(val uint32, width int) {
		bits |= val << bitPos
		bitPos += width
	}

	packBits(boolBit(hasWithdrawals), 1)      // bit 0
	packBits(uint32(diffLen), 6)              // bits 1-6
	packBits(uint32(numberLen), 4)            // bits 7-10
	packBits(uint32(gasLimitLen), 4)          // bits 11-14
	packBits(uint32(gasUsedLen), 4)           // bits 15-18
	packBits(uint32(timestampLen), 4)         // bits 19-22
	packBits(uint32(nonceLen), 4)             // bits 23-26
	packBits(boolBit(hasBaseFee), 1)          // bit 27
	packBits(boolBit(hasBlobGasUsed), 1)      // bit 28
	packBits(boolBit(hasExcessBlobGas), 1)    // bit 29
	packBits(boolBit(hasParentBeaconRoot), 1) // bit 30
	packBits(boolBit(hasExtraFields), 1)      // bit 31 — extra_fields presence

	if bitPos != 32 {
		return nil, fmt.Errorf("headerCompactBytes: internal error: bitPos=%d", bitPos)
	}

	out := make([]byte, 0, 600)

	// 1. Bitfield (4 bytes LE).
	out = append(out, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))

	// 2. Verbatim B256/Address fields.
	out = append(out, h.ParentHash.Bytes()...) // parent_hash
	out = append(out, emptyOmmerHash...)       // ommers_hash (always empty for genesis / dev)
	out = append(out, h.Coinbase.Bytes()...)   // beneficiary (20 bytes)
	out = append(out, h.Root.Bytes()...)       // state_root
	out = append(out, emptyTrieRoot...)        // transactions_root
	out = append(out, emptyTrieRoot...)        // receipts_root

	// 3. withdrawals_root (specialized Option<B256>).
	if hasWithdrawals {
		out = append(out, h.WithdrawalsHash.Bytes()...)
	}

	// 4. logs_bloom (256 bytes).
	out = append(out, h.Bloom.Bytes()...)

	// 5. Compact numeric fields.
	out = appendU256Compact(out, h.Difficulty)     // difficulty
	out = appendU64Compact(out, h.Number.Uint64()) // number
	out = appendU64Compact(out, h.GasLimit)        // gas_limit
	out = appendU64Compact(out, h.GasUsed)         // gas_used
	out = appendU64Compact(out, h.Time)            // timestamp

	// 6. mix_hash (B256, 32 bytes verbatim).
	out = append(out, h.MixDigest.Bytes()...)

	// 7. nonce (compact).
	out = appendU64Compact(out, h.Nonce.Uint64())

	// 8. base_fee_per_gas (Option<u64>, non-specialized: varuint(len) + BE bytes).
	if hasBaseFee {
		bfBytes := u64CompactBE(h.BaseFee.Uint64())
		out = appendVarUint(out, uint64(len(bfBytes)))
		out = append(out, bfBytes...)
	}

	// 9. blob_gas_used (Option<u64>).
	if hasBlobGasUsed {
		bgBytes := u64CompactBE(*h.BlobGasUsed)
		out = appendVarUint(out, uint64(len(bgBytes)))
		out = append(out, bgBytes...)
	}

	// 10. excess_blob_gas (Option<u64>).
	if hasExcessBlobGas {
		ebgBytes := u64CompactBE(*h.ExcessBlobGas)
		out = appendVarUint(out, uint64(len(ebgBytes)))
		out = append(out, ebgBytes...)
	}

	// 11. parent_beacon_block_root (specialized Option<B256>).
	if hasParentBeaconRoot {
		out = append(out, h.ParentBeaconRoot.Bytes()...)
	}

	// 12. extra_fields = Some(HeaderExt{requests_hash: Some(B256)}) when
	//     Prague is active. Non-specialized Option<T> form: varuint(len) ||
	//     inner. See doc-comment header for the wire-format rationale.
	if hasExtraFields {
		var inner []byte
		inner = append(inner, 0x01) // bitflag: requests_hash = Some
		inner = append(inner, h.RequestsHash.Bytes()...)
		out = appendVarUint(out, uint64(len(inner)))
		out = append(out, inner...)
	}

	// 13. extra_data (Bytes, verbatim, last field).
	out = append(out, h.Extra...)

	return out, nil
}

// tdCompactBytes encodes total_difficulty = 0 in reth's CompactU256 format.
//
// CompactU256 wraps U256 with a leading 1-byte bitflag header (the length, 0–32).
// For td=0: length=0, so the encoding is a single byte: [0x00].
//
// From reth crates/storage/db-api/src/models/mod.rs: CompactU256 implements
// Compact and writes a 1-byte header (the length) then the minimal BE bytes.
//
// Verification: in the alloy Header compact encoding, `difficulty: U256::ZERO`
// encodes to 0 bytes (just the length=0 field in the bitflag); CompactU256 adds
// the explicit 1-byte length prefix before the BE value.
func tdCompactBytes() []byte {
	// CompactU256(0) = [length_byte=0x00] (no body bytes since length=0).
	return []byte{0x00}
}

// lz4CompressBlock compresses src using LZ4 raw block format (no size prefix),
// matching lz4_flex::block::compress used by reth's NippyJar writer.
//
// pierrec/lz4/v4 CompressBlock produces a raw LZ4 block identical to
// lz4_flex::block::compress — no 4-byte size prefix is prepended.
//
// With a CompressBlockBound-sized destination buffer, compression always succeeds
// (n > 0) regardless of input incompressibility.
func lz4CompressBlock(src []byte) ([]byte, error) {
	// Empty input → empty output. Matches lz4_flex::block::compress (Vec::new() in,
	// Vec::new() out) and the writer path that reth's append_account_changeset
	// (Vec::new(), 0) exercises when appending an empty block-0 changeset row.
	if len(src) == 0 {
		return nil, nil
	}
	bound := lz4.CompressBlockBound(len(src))
	dst := make([]byte, bound)
	var c lz4.Compressor
	n, err := c.CompressBlock(src, dst)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// Should never happen with a CompressBlockBound-sized buffer for non-empty src.
		return nil, fmt.Errorf("lz4CompressBlock: unexpected n=0 for src len=%d", len(src))
	}
	return dst[:n], nil
}

// emptyOmmerHash is keccak256(rlp([])) = EMPTY_OMMER_ROOT_HASH in reth.
var emptyOmmerHash = mustDecodeHex("1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347")

// emptyTrieRoot is keccak256(rlp("")) = EMPTY_TRIE_ROOT_HASH.
var emptyTrieRoot = mustDecodeHex("56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

// --- helpers ---

func mustDecodeHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var n byte
		for j, c := range s[i : i+2] {
			var v byte
			switch {
			case c >= '0' && c <= '9':
				v = byte(c - '0')
			case c >= 'a' && c <= 'f':
				v = byte(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v = byte(c-'A') + 10
			default:
				panic("mustDecodeHex: invalid hex char")
			}
			if j == 0 {
				n = v << 4
			} else {
				n |= v
			}
		}
		b[i/2] = n
	}
	return b
}

func boolBit(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}

// u64CompactLen returns how many bytes u64 v needs in big-endian minimal encoding.
func u64CompactLen(v uint64) int {
	if v == 0 {
		return 0
	}
	n := 8
	for n > 0 && (v>>(uint(n-1)*8)) == 0 {
		n--
	}
	return n
}

// u256CompactLen returns how many bytes a big.Int v needs (0..32).
func u256CompactLen(v *big.Int) int {
	if v == nil || v.Sign() == 0 {
		return 0
	}
	bs := v.Bytes()
	return len(bs)
}

// u64CompactBE returns the big-endian minimal bytes for v.
func u64CompactBE(v uint64) []byte {
	if v == 0 {
		return nil
	}
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], v)
	i := 0
	for i < 7 && raw[i] == 0 {
		i++
	}
	return raw[i:]
}

func appendU64Compact(dst []byte, v uint64) []byte {
	return append(dst, u64CompactBE(v)...)
}

func appendU256Compact(dst []byte, v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return dst
	}
	return append(dst, v.Bytes()...)
}

func appendVarUint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendLE64(dst []byte, v uint64) []byte {
	return append(dst,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

func appendLE32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
