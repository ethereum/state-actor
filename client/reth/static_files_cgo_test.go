//go:build cgo_reth

package reth

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	lz4 "github.com/pierrec/lz4/v4"
)

// makeGenesisHeader returns a Cancun-enabled genesis header suitable for
// testing WriteStaticFiles. Fields match a typical --dev genesis:
//
//   - gas_limit  = 30_000_000 (4 bytes compact: 0x01C9C380)
//   - base_fee   = 1_000_000_000 (1 GWei, 4 bytes compact)
//   - blob fields = Some(0)
//   - parent_beacon_block_root = Some(zero)
//   - withdrawals_root         = Some(empty-trie-root)
func makeGenesisHeader() *types.Header {
	emptyRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	pbr := common.Hash{}
	blobGas := uint64(0)

	return &types.Header{
		ParentHash:       common.Hash{},
		UncleHash:        common.HexToHash("0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347"),
		Coinbase:         common.Address{},
		Root:             emptyRoot,
		TxHash:           emptyRoot,
		ReceiptHash:      emptyRoot,
		Bloom:            types.Bloom{},
		Difficulty:       big.NewInt(0),
		Number:           big.NewInt(0),
		GasLimit:         30_000_000,
		GasUsed:          0,
		Time:             0,
		Extra:            []byte{},
		MixDigest:        common.Hash{},
		Nonce:            types.BlockNonce{},
		BaseFee:          big.NewInt(1_000_000_000),
		WithdrawalsHash:  &emptyRoot,
		BlobGasUsed:      &blobGas,
		ExcessBlobGas:    &blobGas,
		ParentBeaconRoot: &pbr,
	}
}

// staticFileName builds the expected filename (without extension) for the given segment.
func staticFileName(segName string) string {
	return fmt.Sprintf("static_file_%s_0_%d", segName, blockRangeEnd)
}

// decompressLZ4Block decompresses a raw LZ4 block (no size prefix) using the same
// algorithm as lz4_flex::decompress. It retries with a doubling output buffer,
// matching reth's NippyJar decompressor.
func decompressLZ4Block(compressed []byte) ([]byte, error) {
	for multiplier := 1; multiplier <= 16; multiplier *= 2 {
		buf := make([]byte, multiplier*len(compressed))
		n, err := lz4.UncompressBlock(compressed, buf)
		if err == nil {
			return buf[:n], nil
		}
	}
	// Last attempt with a large fixed buffer
	buf := make([]byte, 65536)
	n, err := lz4.UncompressBlock(compressed, buf)
	if err != nil {
		return nil, fmt.Errorf("decompressLZ4Block: %w", err)
	}
	return buf[:n], nil
}

// TestWriteStaticFilesGenesis checks that WriteStaticFiles creates all expected
// files with the correct structure.
//
// Since reth requires LZ4-compressed column data, the data file contains
// compressed bytes. The test decompresses col0 to verify the header bitfield
// and checks col1/col2 by decompressing them independently.
func TestWriteStaticFilesGenesis(t *testing.T) {
	tmp := t.TempDir()
	header := makeGenesisHeader()

	if err := WriteStaticFiles(tmp, header); err != nil {
		t.Fatalf("WriteStaticFiles: %v", err)
	}

	sfDir := filepath.Join(tmp, staticFilesDir)

	// --- verify all expected files exist ---
	// Data file has NO extension; .off and .conf retain extensions.
	// Change-based segments additionally have a .csoff sidecar.
	segments := []struct {
		name        string
		columns     uint64
		changeBased bool
	}{
		{"headers", 3, false},
		{"transactions", 1, false},
		{"receipts", 1, false},
		{"transaction-senders", 1, false},
		{"account-change-sets", 1, true},
		{"storage-change-sets", 1, true},
	}

	for _, seg := range segments {
		base := filepath.Join(sfDir, staticFileName(seg.name))
		// Data file: no extension
		if _, err := os.Stat(base); err != nil {
			t.Errorf("missing data file %s: %v", staticFileName(seg.name), err)
		}
		for _, ext := range []string{".off", ".conf"} {
			if _, err := os.Stat(base + ext); err != nil {
				t.Errorf("missing %s%s: %v", staticFileName(seg.name), ext, err)
			}
		}
		if seg.changeBased {
			if _, err := os.Stat(base + ".csoff"); err != nil {
				t.Errorf("missing %s.csoff: %v", staticFileName(seg.name), err)
			}
		}
	}

	// --- headers segment: read compressed data file ---
	headersData := filepath.Join(sfDir, staticFileName("headers"))
	sfBytes, err := os.ReadFile(headersData)
	if err != nil {
		t.Fatalf("read headers data file: %v", err)
	}

	// --- headers .off: parse column offsets ---
	headersOff := filepath.Join(sfDir, staticFileName("headers")+".off")
	offBytes, err := os.ReadFile(headersOff)
	if err != nil {
		t.Fatalf("read headers .off: %v", err)
	}
	const wantOffLen = 1 + (3+1)*8 // 33 bytes
	if len(offBytes) != wantOffLen {
		t.Fatalf("headers .off: len=%d, want %d", len(offBytes), wantOffLen)
	}
	if offBytes[0] != 8 {
		t.Errorf("headers .off: offset_size byte = %d, want 8", offBytes[0])
	}
	off0 := binary.LittleEndian.Uint64(offBytes[1:9])
	off1 := binary.LittleEndian.Uint64(offBytes[9:17])
	off2 := binary.LittleEndian.Uint64(offBytes[17:25])
	offEnd := binary.LittleEndian.Uint64(offBytes[25:33])

	// offEnd should equal total data file size
	if offEnd != uint64(len(sfBytes)) {
		t.Errorf("headers .off: last offset = %d, want %d (= data file size)", offEnd, len(sfBytes))
	}

	// --- decompress each column ---
	col0Compressed := sfBytes[off0:off1]
	col1Compressed := sfBytes[off1:off2]
	col2Compressed := sfBytes[off2:offEnd]

	col0, err := decompressLZ4Block(col0Compressed)
	if err != nil {
		t.Fatalf("decompress col0 (header compact): %v", err)
	}
	col1, err := decompressLZ4Block(col1Compressed)
	if err != nil {
		t.Fatalf("decompress col1 (CompactU256 td): %v", err)
	}
	col2, err := decompressLZ4Block(col2Compressed)
	if err != nil {
		t.Fatalf("decompress col2 (block hash B256): %v", err)
	}

	// --- col0: verify bitfield ---
	// Genesis with Cancun fields set and gas_limit=30M (4 bytes = 0x1C9C380):
	//   bit 0 (withdrawals_root): 1
	//   bits 1-6 (difficulty_len = 0): 000000
	//   bits 7-10 (number_len = 0): 0000
	//   bits 11-14 (gas_limit_len = 4): 0100  ← bit 13 set
	//   bits 15-18 (gas_used_len = 0): 0000
	//   bits 19-22 (timestamp_len = 0): 0000
	//   bits 23-26 (nonce_len = 0): 0000
	//   bit 27 (base_fee): 1
	//   bit 28 (blob_gas_used): 1
	//   bit 29 (excess_blob_gas): 1
	//   bit 30 (parent_beacon_block_root): 1
	//   bit 31 (extra_fields): 0
	//
	// Raw uint32 (LSB-first):
	//   bits 31..0 = 0_1111_0000_0000_0000_0010_0000_0000_0001
	//   = 0x78002001 → bytes LE = [0x01, 0x20, 0x00, 0x78]
	wantBitfield := []byte{0x01, 0x20, 0x00, 0x78}
	if len(col0) < 4 {
		t.Fatalf("col0 (header compact) too short for bitfield: %d bytes", len(col0))
	}
	if got := col0[:4]; !equalBytes(got, wantBitfield) {
		t.Errorf("col0 bitfield = %#x, want %#x", got, wantBitfield)
	}

	// Compact header for a Cancun genesis is at least 536 bytes uncompressed
	const minHeaderCompactSize = 536
	if len(col0) < minHeaderCompactSize {
		t.Errorf("col0 (header compact) uncompressed: %d bytes, want >= %d", len(col0), minHeaderCompactSize)
	}

	// --- col1: CompactU256(td=0) = [0x00] ---
	if len(col1) != 1 || col1[0] != 0x00 {
		t.Errorf("col1 (CompactU256 td) = %#x, want [0x00]", col1)
	}

	// --- col2: B256 block hash (32 bytes) ---
	if len(col2) != 32 {
		t.Errorf("col2 (block hash) uncompressed: %d bytes, want 32", len(col2))
	}
	expectedHash := header.Hash()
	if got := common.BytesToHash(col2); got != expectedHash {
		t.Errorf("col2 block hash = %s, want %s", got.Hex(), expectedHash.Hex())
	}

	// --- empty segments: data file must be empty (0 bytes), .off must be 9 bytes ---
	for _, seg := range []string{"transactions", "receipts", "transaction-senders"} {
		base := filepath.Join(sfDir, staticFileName(seg))

		dataInfo, err := os.Stat(base)
		if err != nil {
			t.Errorf("%s data file stat: %v", seg, err)
			continue
		}
		if dataInfo.Size() != 0 {
			t.Errorf("%s data file: size=%d, want 0 (empty segment)", seg, dataInfo.Size())
		}

		offData, err := os.ReadFile(base + ".off")
		if err != nil {
			t.Errorf("%s .off read: %v", seg, err)
			continue
		}
		// rows=0: offset_size byte + 1 final offset (all zeros) = 9 bytes.
		if len(offData) != 9 {
			t.Errorf("%s .off: len=%d, want 9", seg, len(offData))
		}
		if offData[0] != 8 {
			t.Errorf("%s .off: offset_size byte = %d, want 8", seg, offData[0])
		}
	}

	// --- .conf file for headers ---
	// NippyJar bincode starts with: version(u64 LE) = 1 → [1, 0, 0, 0, 0, 0, 0, 0]
	headersConf := filepath.Join(sfDir, staticFileName("headers")+".conf")
	confBytes, err := os.ReadFile(headersConf)
	if err != nil {
		t.Fatalf("read headers .conf: %v", err)
	}
	if len(confBytes) < 8 {
		t.Fatalf("headers .conf too short: %d bytes", len(confBytes))
	}
	version := binary.LittleEndian.Uint64(confBytes[:8])
	if version != nippyJarVersion {
		t.Errorf("headers .conf: NippyJar version = %d, want %d", version, nippyJarVersion)
	}

	// For non-empty segments with LZ4 compression the conf tail is:
	//   columns(u64 LE=8) + rows(u64 LE=8) + Some(1byte) + Lz4_discriminant(u32 LE=4) + max_row_size(u64 LE=8)
	// = 8 + 8 + 1 + 4 + 8 = 29 bytes
	const confTailLen = 8 + 8 + 1 + 4 + 8
	if len(confBytes) < confTailLen {
		t.Fatalf("headers .conf too short for tail check: %d bytes", len(confBytes))
	}
	tail := confBytes[len(confBytes)-confTailLen:]
	cols := binary.LittleEndian.Uint64(tail[0:8])
	rows := binary.LittleEndian.Uint64(tail[8:16])
	compressorPresence := tail[16]
	compressorVariant := binary.LittleEndian.Uint32(tail[17:21])
	maxRowSz := binary.LittleEndian.Uint64(tail[21:29])

	if cols != 3 {
		t.Errorf("headers .conf: columns = %d, want 3", cols)
	}
	if rows != 1 {
		t.Errorf("headers .conf: rows = %d, want 1", rows)
	}
	if compressorPresence != 0x01 {
		t.Errorf("headers .conf: compressor presence = %#x, want 0x01 (Some)", compressorPresence)
	}
	if compressorVariant != 1 {
		t.Errorf("headers .conf: compressor variant = %d, want 1 (Lz4)", compressorVariant)
	}
	// max_row_size = uncompressed total = len(col0)+len(col1)+len(col2)
	wantMaxRowSz := uint64(len(col0) + len(col1) + len(col2))
	if maxRowSz != wantMaxRowSz {
		t.Errorf("headers .conf: max_row_size = %d, want %d (uncompressed total)", maxRowSz, wantMaxRowSz)
	}
}

// TestHeaderCompactBytesGenesis checks structural properties of the compact encoding
// for a minimal genesis header (no optional Cancun fields).
func TestHeaderCompactBytesGenesis(t *testing.T) {
	h := &types.Header{
		ParentHash: common.Hash{},
		Difficulty: big.NewInt(0),
		Number:     big.NewInt(0),
		GasLimit:   30_000_000,
		GasUsed:    0,
		Time:       0,
		Extra:      []byte{},
		MixDigest:  common.Hash{},
		BaseFee:    big.NewInt(1_000_000_000),
	}

	b, err := headerCompactBytes(h)
	if err != nil {
		t.Fatalf("headerCompactBytes: %v", err)
	}

	// Minimum: 4 (bitfield) + 32+32+20+32+32+32 (verbatim) + 256 (bloom)
	//          + (compact numeric: gas_limit=3, base_fee=4) + 32 (mix_hash)
	//          = 4 + 180 + 256 + 7 + 32 = 479 bytes.
	const minSize = 479
	if len(b) < minSize {
		t.Errorf("headerCompactBytes: %d bytes, want >= %d", len(b), minSize)
	}

	// Bitfield byte 0:
	//   bit 0 (withdrawals_root=None): 0
	//   bits 1-6 (difficulty_len=0): 000000
	//   bits 7 (number_len bit 0): 0
	//   → byte 0 = 0x00
	if b[0] != 0x00 {
		t.Errorf("bitfield[0] = %#x, want 0x00", b[0])
	}

	// gas_limit_len=4 (30M = 0x1C9C380, 4 bytes) occupies bits 11-14.
	// byte 1 = bits 8-15:
	//   bits 8-10  = number_len bits 1-3 = 0
	//   bits 11-14 = gas_limit_len = 4 = 0b0100 → bit 13 set
	//   bit 15 = gas_used_len bit 0 = 0
	// bit 13 is bit 5 of byte 1 → byte 1 = 0b00100000 = 0x20
	if b[1] != 0x20 {
		t.Errorf("bitfield[1] = %#x, want 0x20 (gas_limit_len=4 in bits 11-14)", b[1])
	}
}

// TestHeaderCompactBytesPragueExtraFields verifies that a Prague-active
// header (RequestsHash set) emits the Option<HeaderExt> wire-form at the
// tail per reth-codecs-0.3.1 (lib.rs Option<T>::to_compact for
// the non-specialized branch):
//
//   - bit 31 of the LE bitfield (byte 3, bit 7) is set
//   - the appended payload is exactly 34 bytes:
//     varuint(33) = 0x21
//     inner bitflag = 0x01 (requests_hash = Some)
//     32 bytes raw RequestsHash
//
// The varuint prefix is what reth-codecs requires for any Option<T> over
// a custom (non-specialized) Compact-derived struct — specialized Options
// (Option<B256>, Option<u64>) elsewhere in the parent header skip it.
// Omitting the prefix mis-aligns reth's decoder and panics in
// `Compact for [u8;N]::from_compact` at lib.rs.
func TestHeaderCompactBytesPragueExtraFields(t *testing.T) {
	// Build the minimal genesis header from TestHeaderCompactBytesGenesis,
	// then add the Prague RequestsHash. Compare lengths and byte structure.
	baseHdr := &types.Header{
		ParentHash: common.Hash{},
		Difficulty: big.NewInt(0),
		Number:     big.NewInt(0),
		GasLimit:   30_000_000,
		GasUsed:    0,
		Time:       0,
		Extra:      []byte{},
		MixDigest:  common.Hash{},
		BaseFee:    big.NewInt(1_000_000_000),
	}
	baseBytes, err := headerCompactBytes(baseHdr)
	if err != nil {
		t.Fatalf("headerCompactBytes (base): %v", err)
	}

	pragueHdr := *baseHdr
	emptyReq := types.EmptyRequestsHash
	pragueHdr.RequestsHash = &emptyReq
	pragueBytes, err := headerCompactBytes(&pragueHdr)
	if err != nil {
		t.Fatalf("headerCompactBytes (prague): %v", err)
	}

	// Prague encoding must be exactly 34 bytes longer: 1 byte varuint(33)
	// + 1 byte inner bitflag + 32 bytes B256.
	if len(pragueBytes)-len(baseBytes) != 34 {
		t.Errorf("Prague-active byte delta = %d, want 34 (varuint + inner bitflag + B256)",
			len(pragueBytes)-len(baseBytes))
	}

	// Bit 31 in the LE bitfield = bit 7 of byte 3.
	if pragueBytes[3]&0x80 == 0 {
		t.Errorf("bitfield byte 3 = %#x, want bit 7 (extra_fields presence) set", pragueBytes[3])
	}
	if baseBytes[3]&0x80 != 0 {
		t.Errorf("non-Prague header has extra_fields bit set: byte 3 = %#x", baseBytes[3])
	}

	// Tail layout: extra_data is empty for both headers, so the last 34
	// bytes of pragueBytes must be the Option<HeaderExt> Some encoding:
	// [0x21, 0x01, RequestsHash[0..32)].
	tail := pragueBytes[len(pragueBytes)-34:]
	if tail[0] != 0x21 {
		t.Errorf("HeaderExt varuint length prefix = %#x, want 0x21 (varuint(33))", tail[0])
	}
	if tail[1] != 0x01 {
		t.Errorf("HeaderExt inner bitflag = %#x, want 0x01 (requests_hash=Some)", tail[1])
	}
	if [32]byte(tail[2:]) != emptyReq {
		t.Errorf("HeaderExt requests_hash bytes:\n got  %x\n want %x", tail[2:], emptyReq)
	}
}

// TestBuildOffsetsFileEmpty verifies the 9-byte layout for a rows=0 segment.
func TestBuildOffsetsFileEmpty(t *testing.T) {
	off := buildOffsetsFile(1, nil)
	if len(off) != 9 {
		t.Errorf("len = %d, want 9", len(off))
	}
	if off[0] != 8 {
		t.Errorf("offset_size = %d, want 8", off[0])
	}
	lastOff := binary.LittleEndian.Uint64(off[1:])
	if lastOff != 0 {
		t.Errorf("final offset = %d, want 0", lastOff)
	}
}

// TestBuildOffsetsFileOneRow verifies offsets for a 1-row 3-column segment
// with known column sizes.
func TestBuildOffsetsFileOneRow(t *testing.T) {
	col0 := make([]byte, 10)
	col1 := make([]byte, 5)
	col2 := make([]byte, 3)

	off := buildOffsetsFile(3, [][]byte{col0, col1, col2})
	// Expected: 1 + (3+1)*8 = 33 bytes
	if len(off) != 33 {
		t.Fatalf("len = %d, want 33", len(off))
	}
	if off[0] != 8 {
		t.Errorf("offset_size = %d, want 8", off[0])
	}

	offsets := make([]uint64, 4)
	for i := range offsets {
		offsets[i] = binary.LittleEndian.Uint64(off[1+i*8:])
	}

	if offsets[0] != 0 {
		t.Errorf("off[0] = %d, want 0", offsets[0])
	}
	if offsets[1] != 10 {
		t.Errorf("off[1] = %d, want 10", offsets[1])
	}
	if offsets[2] != 15 {
		t.Errorf("off[2] = %d, want 15", offsets[2])
	}
	if offsets[3] != 18 {
		t.Errorf("off[3] = %d, want 18 (total data size)", offsets[3])
	}
}

// TestTdCompactBytes checks that CompactU256(0) encodes to [0x00].
func TestTdCompactBytes(t *testing.T) {
	b := tdCompactBytes()
	if len(b) != 1 || b[0] != 0x00 {
		t.Errorf("tdCompactBytes() = %#x, want [0x00]", b)
	}
}

// TestBuildSegmentHeaderBytesHeaders checks that headers SegmentHeader uses tx_range=None.
func TestBuildSegmentHeaderBytesHeaders(t *testing.T) {
	b := buildSegmentHeaderBytes(segHeaders, 0)

	// expected_block_range: start=0 (8 LE), end=499999 (8 LE) = 16 bytes
	// block_range: Some (0x01) + start=0 (8) + end=0 (8) = 17 bytes
	// tx_range: None (0x00) = 1 byte
	// segment: 0 (4 LE)
	// Total = 16 + 17 + 1 + 4 = 38 bytes
	const wantLen = 38
	if len(b) != wantLen {
		t.Fatalf("len = %d, want %d", len(b), wantLen)
	}

	// tx_range presence byte (at offset 33) should be 0x00 (None).
	txRangeByte := b[16+1+16] // after expected_block_range(16) + Some(1) + block_range(16)
	if txRangeByte != 0x00 {
		t.Errorf("tx_range byte = %#x, want 0x00 (None) for headers segment", txRangeByte)
	}

	// segment discriminant (last 4 bytes) = 0 for Headers.
	segDiscr := binary.LittleEndian.Uint32(b[len(b)-4:])
	if segDiscr != 0 {
		t.Errorf("segment discriminant = %d, want 0 (Headers)", segDiscr)
	}
}

// TestBuildSegmentHeaderBytesTransactions checks that transactions SegmentHeader uses tx_range=Some.
func TestBuildSegmentHeaderBytesTransactions(t *testing.T) {
	b := buildSegmentHeaderBytes(segTransactions, 0)

	// expected_block_range(16) + Some(1)+block_range(16) + Some(1)+tx_range(16) + u32(4)
	const wantLen = 16 + 17 + 17 + 4
	if len(b) != wantLen {
		t.Fatalf("len = %d, want %d", len(b), wantLen)
	}

	// tx_range presence byte (at offset 33) should be 0x01 (Some).
	txRangeByte := b[16+1+16]
	if txRangeByte != 0x01 {
		t.Errorf("tx_range byte = %#x, want 0x01 (Some) for transactions segment", txRangeByte)
	}

	// segment discriminant = 1 for Transactions.
	segDiscr := binary.LittleEndian.Uint32(b[len(b)-4:])
	if segDiscr != 1 {
		t.Errorf("segment discriminant = %d, want 1 (Transactions)", segDiscr)
	}
}

// TestBuildSegmentHeaderBytesChangesets verifies the 5-field SegmentHeader
// layout for change-based segments (AccountChangeSets, StorageChangeSets):
// expected_block_range(16) + Some(block_range)(17) + None(tx_range)(1)
// + segment_u32(4) + changeset_offsets_len_u64(8) = 46 bytes.
func TestBuildSegmentHeaderBytesChangesets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seg     staticFileSegment
		wantDis uint32
	}{
		{"account-change-sets", segAccountChangeSets, 4},
		{"storage-change-sets", segStorageChangeSets, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := buildSegmentHeaderBytes(tc.seg, 1)

			// expected_block_range(16) + Some(1)+block_range(16) + None(1) + u32(4) + u64(8)
			const wantLen = 16 + 17 + 1 + 4 + 8
			if len(b) != wantLen {
				t.Fatalf("len = %d, want %d", len(b), wantLen)
			}

			// tx_range presence byte at offset 33 must be 0x00 (None).
			if got := b[16+1+16]; got != 0x00 {
				t.Errorf("tx_range byte = %#x, want 0x00 (None) for change-based segment", got)
			}

			// segment discriminant occupies bytes 34..38.
			segDiscr := binary.LittleEndian.Uint32(b[34:38])
			if segDiscr != tc.wantDis {
				t.Errorf("segment discriminant = %d, want %d", segDiscr, tc.wantDis)
			}

			// changeset_offsets_len occupies the trailing 8 bytes; we passed 1.
			csOffLen := binary.LittleEndian.Uint64(b[38:46])
			if csOffLen != 1 {
				t.Errorf("changeset_offsets_len = %d, want 1", csOffLen)
			}
		})
	}
}

// TestWriteStaticFilesChangesetShells verifies the empty bootstrap shell for
// AccountChangeSets and StorageChangeSets — exactly what reth's persistence
// service expects to find at boot so it can append block 1 cleanly.
//
// Each shell consists of:
//   - data file: 0 bytes (1 row, empty content — LZ4 of empty = empty)
//   - .off:      17 bytes ([8, 0×8 (row-0 start), 0×8 (row-0 end = empty)])
//   - .conf:     5-field SegmentHeader with changeset_offsets_len=1, rows=1,
//     columns=1, compressor=Some(Lz4), max_row_size=0
//   - .csoff:    16 bytes (single record [offset=0, num_changes=0])
func TestWriteStaticFilesChangesetShells(t *testing.T) {
	tmp := t.TempDir()
	header := makeGenesisHeader()
	if err := WriteStaticFiles(tmp, header); err != nil {
		t.Fatalf("WriteStaticFiles: %v", err)
	}
	sfDir := filepath.Join(tmp, staticFilesDir)

	for _, tc := range []struct {
		segName string
		segDis  uint32
	}{
		{"account-change-sets", 4},
		{"storage-change-sets", 5},
	} {
		t.Run(tc.segName, func(t *testing.T) {
			base := filepath.Join(sfDir, staticFileName(tc.segName))

			// Data file: 0 bytes (empty content for the single row).
			dataInfo, err := os.Stat(base)
			if err != nil {
				t.Fatalf("data file stat: %v", err)
			}
			if dataInfo.Size() != 0 {
				t.Errorf("data file size = %d, want 0", dataInfo.Size())
			}

			// .off: 17 bytes. byte 0 = 8 (offset_size). bytes 1..9 = row-0 start = 0.
			// bytes 9..17 = end-of-row-0 = 0 (empty).
			offBytes, err := os.ReadFile(base + ".off")
			if err != nil {
				t.Fatalf(".off read: %v", err)
			}
			if len(offBytes) != 17 {
				t.Errorf(".off len = %d, want 17", len(offBytes))
			}
			if offBytes[0] != 8 {
				t.Errorf(".off[0] = %d, want 8", offBytes[0])
			}
			if got := binary.LittleEndian.Uint64(offBytes[1:9]); got != 0 {
				t.Errorf(".off row-0 start = %d, want 0", got)
			}
			if got := binary.LittleEndian.Uint64(offBytes[9:17]); got != 0 {
				t.Errorf(".off row-0 end   = %d, want 0", got)
			}

			// .conf: parse the trailing fixed-size fields.
			// Tail layout for non-empty compressed segments:
			//   columns(u64) + rows(u64) + Some(1) + Lz4_variant(u32) + max_row_size(u64)
			confBytes, err := os.ReadFile(base + ".conf")
			if err != nil {
				t.Fatalf(".conf read: %v", err)
			}
			const confTailLen = 8 + 8 + 1 + 4 + 8
			if len(confBytes) < 8+confTailLen {
				t.Fatalf(".conf too short: %d bytes", len(confBytes))
			}
			version := binary.LittleEndian.Uint64(confBytes[:8])
			if version != nippyJarVersion {
				t.Errorf("NippyJar version = %d, want %d", version, nippyJarVersion)
			}
			tail := confBytes[len(confBytes)-confTailLen:]
			cols := binary.LittleEndian.Uint64(tail[0:8])
			rows := binary.LittleEndian.Uint64(tail[8:16])
			compressorPresence := tail[16]
			compressorVariant := binary.LittleEndian.Uint32(tail[17:21])
			maxRowSize := binary.LittleEndian.Uint64(tail[21:29])
			if cols != 1 {
				t.Errorf(".conf columns = %d, want 1", cols)
			}
			if rows != 1 {
				t.Errorf(".conf rows = %d, want 1 (the empty block-0 shell)", rows)
			}
			if compressorPresence != 0x01 {
				t.Errorf(".conf compressor presence = %#x, want 0x01 (Some)", compressorPresence)
			}
			if compressorVariant != 1 {
				t.Errorf(".conf compressor variant = %d, want 1 (Lz4)", compressorVariant)
			}
			if maxRowSize != 0 {
				t.Errorf(".conf max_row_size = %d, want 0 (empty content)", maxRowSize)
			}

			// The bytes between version(8) and the tail are the user_header
			// (5-field SegmentHeader for change-based segments). Length =
			// 16 (expected_block_range) + 17 (Some block_range) + 1 (None tx_range)
			// + 4 (segment u32) + 8 (changeset_offsets_len) = 46.
			userHeader := confBytes[8 : len(confBytes)-confTailLen]
			if len(userHeader) != 46 {
				t.Fatalf("user_header len = %d, want 46", len(userHeader))
			}
			segDiscr := binary.LittleEndian.Uint32(userHeader[34:38])
			if segDiscr != tc.segDis {
				t.Errorf("segment discriminant = %d, want %d", segDiscr, tc.segDis)
			}
			csOffLen := binary.LittleEndian.Uint64(userHeader[38:46])
			if csOffLen != 1 {
				t.Errorf("changeset_offsets_len = %d, want 1", csOffLen)
			}

			// .csoff sidecar: 16 bytes of zeros (single record: offset=0, num_changes=0).
			csoffBytes, err := os.ReadFile(base + ".csoff")
			if err != nil {
				t.Fatalf(".csoff read: %v", err)
			}
			if len(csoffBytes) != 16 {
				t.Errorf(".csoff len = %d, want 16", len(csoffBytes))
			}
			for i, b := range csoffBytes {
				if b != 0 {
					t.Errorf(".csoff[%d] = %#x, want 0x00", i, b)
				}
			}
		})
	}
}

// equalBytes compares two byte slices.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
