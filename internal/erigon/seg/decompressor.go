package seg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"os"

	"golang.org/x/sys/unix"
)

// OffsetEntry is one (key, value, keyOffset, valueOffset) tuple yielded
// by Decompressor.Iterate. Offsets are byte positions RELATIVE TO THE
// START OF THE BIT-STREAM (i.e., after the V1 header + data-section
// header + position dictionary). This matches Erigon's `Getter.dataP`
// semantics — `MakeGetter` slices `d.data[d.wordsStart:]` so all
// downstream offsets are relative to `wordsStart`.
//
// Which offset to feed each accessor builder depends on the accessor's
// underlying file format:
//
//   - BTree (.bt) and HashMap (.kvi) accessors index INTO (key,value)-pair
//     .kv files. The reader path (BtIndex.dataLookup at
//     db/datastruct/btindex/btree_index.go:531, RecSplit.Lookup) does
//     `g.Reset(off); g.Next(nil) /*=key*/; g.Next(nil) /*=value*/`, so
//     `off` MUST be the offset of the KEY's length-prefix — i.e.
//     KeyOffset. Upstream's reference BT builder
//     (btree_index.go:419-445) captures `pos` BEFORE `kv.Next(key[:0])`
//     and stores it via `iw.AddKey(key, pos, keep)`, confirming the
//     pre-key offset is what gets indexed.
//
//   - InvertedIndex (.efi / .vi) accessors index INTO single-word .ef /
//     .v files (one logical entry = one seg word, no key/value
//     alternation). Per db/state/simple_accessor_builder.go:194-216
//     they consume ValueOffset semantics — the offset of the single
//     word at each logical position.
//
// Feeding ValueOffset to a BTree accessor positions the Huffman cursor
// MID-entry (between a key and its value); decoding then mis-parses
// garbage as a position code and walks dataP past EOF — manifesting as
// `panic: runtime error: index out of range [N] with length N` from
// the daemon's first state lookup. See exec3_serial.go:349 in upstream
// for the panic site we previously hit because of this mix-up.
type OffsetEntry struct {
	// Key is the decoded key bytes (the even-index word). Backing
	// storage is owned by the iterator and may be overwritten on the
	// next iteration — copy if you need to retain past the next yield.
	Key []byte
	// Value is the decoded value bytes (the odd-index word). Same
	// retention rules as Key.
	Value []byte
	// KeyOffset is the byte offset of the key's length-prefix bits,
	// relative to the start of the bit-stream. THIS is the value to
	// feed BTree (`btindex.Writer.AddKey`) and HashMap
	// (`recsplit.Writer.AddKey`) accessor builders over key/value-pair
	// .kv files. Source of truth: upstream
	// db/datastruct/btindex/btree_index.go:432.
	KeyOffset uint64
	// ValueOffset is the byte offset of the value's length-prefix bits,
	// relative to the start of the bit-stream. Use this ONLY for
	// InvertedIndex-style accessors over single-word .ef / .v files
	// (per db/state/simple_accessor_builder.go:194-216). Do NOT feed it
	// to BTree or HashMap accessors over (key,value)-pair .kv files —
	// it produces a runtime panic in Erigon's daemon (see KeyOffset
	// doc above).
	ValueOffset uint64
}

// ErrCorruptedFile signals a wire-format violation (truncated header,
// length-prefix overflow, missing terminator, etc.). The caller's
// `iter.Seq2` consumer receives this as the err half of the (entry, err)
// yield.
var ErrCorruptedFile = errors.New("seg: corrupted file")

// Decompressor opens a .kv-style file READ-ONLY via mmap. The .kv can be
// tens of GiB at bench scale (e.g. a 44 GiB commitment.kv at 100 GB
// account-heavy); os.ReadFile would pull the whole file into ANONYMOUS heap,
// and Phase-5b builds two domains concurrently — that combination OOM-killed
// a 100 GB run at ~120 GiB anon-rss. mmap keeps the bytes as reclaimable,
// file-backed pages (file-rss), so anon stays O(1) regardless of .kv size.
type Decompressor struct {
	filePath   string
	data       []byte // mmap'd file contents (munmap on Close)
	wordsCount uint64
	emptyCount uint64
	// Pattern Huffman tables are absent in v1's no-pattern fast path;
	// NewDecompressor rejects files with a non-empty pattern dictionary.
	posList    []*position // in on-disk order
	pos2code   map[uint64]*position
	wordsStart uint64 // byte offset (in d.data) where the Huffman bitstream begins
}

// NewDecompressor opens path and parses the V1 header + position
// dictionary. The pattern dictionary MUST be empty (no-pattern fast
// path); a non-zero patternsSize returns an error.
func NewDecompressor(path string) (*Decompressor, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	size := int(st.Size())
	if size < compressedMinSize {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s: file size %d < %d",
			ErrCorruptedFile, path, size, compressedMinSize)
	}
	// READ-ONLY shared mmap; the mapping survives closing the fd.
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("seg: mmap %s: %w", path, err)
	}
	// The .kv is decoded strictly front-to-back exactly once (offset discovery
	// in WriteDomain Pass 2). Hint the kernel: aggressive read-ahead for the
	// sequential scan, and — importantly for the OOM-sensitive Phase 5b — let
	// it drop already-read pages sooner, keeping file-RSS low on a 44 GiB .kv.
	// Advisory only; best-effort, never changes decoded bytes.
	_ = unix.Madvise(data, unix.MADV_SEQUENTIAL)
	// Munmap on any parse error below so a rejected file doesn't leak the
	// mapping.
	ok := false
	defer func() {
		if !ok {
			_ = unix.Munmap(data)
		}
	}()
	d := &Decompressor{filePath: path, data: data}

	// V1 header: byte[0]=version, byte[1]=featureFlagBitmask.
	if d.data[0] != fileCompressionFormatV1 {
		return nil, fmt.Errorf("%w: %s: expected V1 (0x01), got 0x%02x",
			ErrCorruptedFile, path, d.data[0])
	}
	flags := featureFlagBitmask(d.data[1])
	// PageLevelCompression / ExpectMetadata are out of scope in v1; we
	// reject any flag bits to avoid silently mis-decoding.
	if flags != 0 {
		return nil, fmt.Errorf("%w: %s: unsupported feature flags 0x%02x",
			ErrCorruptedFile, path, flags)
	}
	pos := 2

	// Data-section header: 8B wordsCount, 8B emptyCount, 8B patternsSize.
	if len(d.data)-pos < 24 {
		return nil, fmt.Errorf("%w: %s: truncated data header",
			ErrCorruptedFile, path)
	}
	d.wordsCount = binary.BigEndian.Uint64(d.data[pos : pos+8])
	pos += 8
	d.emptyCount = binary.BigEndian.Uint64(d.data[pos : pos+8])
	pos += 8
	patternsSize := binary.BigEndian.Uint64(d.data[pos : pos+8])
	pos += 8
	if patternsSize != 0 {
		return nil, fmt.Errorf(
			"seg: %s: file has non-empty pattern dictionary (patternsSize=%d); "+
				"state-actor v1 only supports the no-pattern fast path",
			path, patternsSize)
	}
	// pos += patternsSize  // (zero in v1)

	// Position dictionary: 8B posSize, then varint pairs.
	if uint64(len(d.data))-uint64(pos) < 8 {
		return nil, fmt.Errorf("%w: %s: truncated posSize",
			ErrCorruptedFile, path)
	}
	posSize := binary.BigEndian.Uint64(d.data[pos : pos+8])
	pos += 8
	if uint64(pos)+posSize > uint64(len(d.data)) {
		return nil, fmt.Errorf(
			"%w: %s: posSize %d overflows file (len=%d, pos=%d)",
			ErrCorruptedFile, path, posSize, len(d.data), pos)
	}

	// Decode position entries: (varint depth, varint pos) pairs. The
	// file holds them in canonical-Huffman order (sorted by
	// bits.Reverse64(code) ascending after Huffman codes were
	// assigned). To reconstruct codes we re-derive them from depths
	// using the same algorithm as buildPositionHuffman: in v1 we don't
	// strictly NEED to decode — Iterate only needs to know each word's
	// length, which is encoded in bits. So we parse + rebuild the
	// Huffman tree the same way the Compressor did.
	dictData := d.data[pos : pos+int(posSize)]
	dictPos := uint64(0)
	var raw []rawPos
	for dictPos < posSize {
		depth, ns := binary.Uvarint(dictData[dictPos:])
		if ns <= 0 {
			return nil, fmt.Errorf("%w: %s: bad pos depth varint",
				ErrCorruptedFile, path)
		}
		dictPos += uint64(ns)
		p, n := binary.Uvarint(dictData[dictPos:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: %s: bad pos value varint",
				ErrCorruptedFile, path)
		}
		dictPos += uint64(n)
		raw = append(raw, rawPos{depth: depth, pos: p})
	}
	pos += int(posSize)

	// Reconstruct the Huffman codes from the depths using the same
	// canonical-Huffman algorithm Erigon uses on the writer side. The
	// file stores entries in the FINAL sort order (by bits.Reverse64(code)
	// after Huffman assignment), so we rebuild the tree from a posMap
	// that recovers the original uses (which we don't actually have on
	// disk — we only have depths). To bypass this, we use the
	// alternative: re-derive codes from depths via canonical Huffman.
	d.posList, d.pos2code = codesFromDepths(raw)

	d.wordsStart = uint64(pos)
	ok = true
	return d, nil
}

// Close munmaps the file's backing pages. After Close, no further Iterate
// calls are valid.
func (d *Decompressor) Close() error {
	if d.data != nil {
		_ = unix.Munmap(d.data)
		d.data = nil
	}
	d.posList = nil
	d.pos2code = nil
	return nil
}

// Iterate yields one OffsetEntry per (key, value) pair. The compressor
// requires an even word count (alternating key/value); odd counts cause
// the final yield to be an error.
//
// Context cancellation is checked at each yield; when ctx.Err() is
// non-nil the iterator yields (zero, ctx.Err()) once and stops.
func (d *Decompressor) Iterate(ctx context.Context) iter.Seq2[OffsetEntry, error] {
	return func(yield func(OffsetEntry, error) bool) {
		if d.data == nil {
			yield(OffsetEntry{}, errors.New("seg: decompressor closed"))
			return
		}
		if d.wordsCount%2 != 0 {
			yield(OffsetEntry{}, fmt.Errorf(
				"seg: %s: odd word count %d; .kv files must alternate key/value",
				d.filePath, d.wordsCount))
			return
		}

		// Match Erigon's Getter semantics: bitstream is the
		// subslice starting at wordsStart, and dataP is relative.
		g := &bitReader{
			data:     d.data[d.wordsStart:],
			dataP:    0,
			posList:  d.posList,
			pos2code: d.pos2code,
		}

		var keyBuf, valBuf []byte
		var keyOff, valOff uint64
		idx := uint64(0)
		for idx < d.wordsCount {
			if ctxErr := ctx.Err(); ctxErr != nil {
				yield(OffsetEntry{}, ctxErr)
				return
			}
			off := g.dataP
			word, err := g.nextWord(nil)
			if err != nil {
				yield(OffsetEntry{}, err)
				return
			}
			if idx%2 == 0 {
				// Key: copy because we'll yield as part of the pair.
				keyBuf = append(keyBuf[:0], word...)
				keyOff = off
			} else {
				valBuf = append(valBuf[:0], word...)
				valOff = off
				if !yield(OffsetEntry{
					Key:         keyBuf,
					Value:       valBuf,
					KeyOffset:   keyOff,
					ValueOffset: valOff,
				}, nil) {
					return
				}
			}
			idx++
		}
	}
}

// Count returns the total word count (alternating key+value, so the
// number of (key, value) pairs is Count()/2). Useful for sizing
// downstream accessor builders.
func (d *Decompressor) Count() uint64 {
	return d.wordsCount
}

// EmptyCount returns the number of empty-length words. Erigon stores
// this for reporting / sanity-checking; state-actor surfaces it for
// tests.
func (d *Decompressor) EmptyCount() uint64 {
	return d.emptyCount
}

// rawPos is a (depth, pos) pair as parsed from the file.
type rawPos struct {
	depth uint64
	pos   uint64
}

// codesFromDepths reconstructs the (code, codeBits) pair for each
// (depth, pos) on-disk entry using the SAME recursive-walk algorithm
// Erigon's Decompressor uses to populate its lookup tables
// (`db/seg/decompress.go:459-505 buildPosTable`). The on-disk order
// matches Erigon's canonical-Huffman emission order, so processing
// entries in disk order during the recursive walk yields the same
// (code, depth) pairs Erigon's writer assigned.
//
// Algorithm: preorder-traverse a binary tree. When the walker's
// current depth equals the next entry's depth, consume that entry and
// assign it the walker's current (code, bits) values. Otherwise
// recurse left (code stays, bits+=1, depth+=1) then right
// (code |= 1<<bits, bits+=1, depth+=1).
//
// Bit ordering note: this matches `buildPosTable` at decompress.go:499-503
// where the recursion is left-first (code stays, bits+=1) then right
// (code |= 1<<bits, bits+=1). This places the FIRST tree decision
// (root → left/right) in bit position 0 of the final code value, the
// SECOND decision in bit position 1, etc. — matching how the writer's
// AddZero/AddOne procedure produces codes (each new bit is appended in
// the LSB position via `code <<= 1; code |= newBit`, but the recursive
// re-derivation chooses to place new bits in the next-higher position
// — both algorithms yield SAME final numerical code values because the
// LSB-first packing on the wire then reads bits back in LSB-first order).
//
// Returns entries in DISK ORDER (with codes filled in) and a
// pos→entry lookup map for the bit-decoder's hot path.
func codesFromDepths(raw []rawPos) ([]*position, map[uint64]*position) {
	list := make([]*position, len(raw))
	m := make(map[uint64]*position, len(raw))
	if len(raw) == 0 {
		return list, m
	}
	var maxDepth uint64
	for _, r := range raw {
		if r.depth > maxDepth {
			maxDepth = r.depth
		}
	}
	idx := 0
	var walk func(code uint64, bits int, depth uint64)
	walk = func(code uint64, bits int, depth uint64) {
		if idx >= len(raw) {
			return
		}
		if depth == raw[idx].depth {
			p := &position{
				pos:      raw[idx].pos,
				depth:    int(depth),
				code:     code,
				codeBits: bits,
			}
			list[idx] = p
			m[raw[idx].pos] = p
			idx++
			return
		}
		if depth > maxDepth {
			return
		}
		// Left: code stays, bits+=1, depth+=1.
		walk(code, bits+1, depth+1)
		// Right: code |= 1<<bits, bits+=1, depth+=1.
		walk(code|(uint64(1)<<uint(bits)), bits+1, depth+1)
	}
	walk(0, 0, 0)
	return list, m
}

// bitReader is the Decompressor-side Huffman + raw-bytes consumer.
// Tracks a (byte, bit) cursor into d.data.
type bitReader struct {
	data     []byte
	dataP    uint64
	dataBit  int // 0..7
	posList  []*position
	pos2code map[uint64]*position
}

// nextPosClean byte-aligns the cursor THEN reads the next Huffman code.
// Mirrors `Getter.nextPosClean` at decompress.go:794-800 — the
// alignment is BEFORE the read, not after. Use this at word boundaries
// where the previous word's data ended on a byte boundary (via the
// writer's flush() call) but the cursor may have left bits stale from
// the previous Huffman emission within that byte.
func (br *bitReader) nextPosClean() (uint64, error) {
	if br.dataBit > 0 {
		br.dataP++
		br.dataBit = 0
	}
	return br.nextPos()
}

// nextPos reads the next Huffman position code. Linear scan over
// posList (acceptable because state-actor v1 has small dictionaries —
// distinct word lengths in a 1K-word fixture is ≤ ~256). The
// production code uses a flat lookup table for speed; we don't need
// that for the v1 fixture sizes.
//
// Bit ordering: the encoder writes code bits LSB-first (bit 0 of code
// → first bit emitted, bit 1 → second, etc). The decoder reconstructs
// the same numerical `code` by reading bits in the same order and
// placing them at LSB-first positions: bit N read goes to position N
// of the accumulator. After codeBits reads the accumulator equals the
// encoder's `code` value verbatim.
func (br *bitReader) nextPos() (uint64, error) {
	const maxAllowedDepth = 50
	var code uint64
	for bits := 1; bits <= maxAllowedDepth; bits++ {
		bit, err := br.readBit()
		if err != nil {
			return 0, err
		}
		code |= uint64(bit) << uint(bits-1)
		for _, p := range br.posList {
			if p.codeBits == bits && p.code == code {
				return p.pos, nil
			}
		}
	}
	return 0, fmt.Errorf("%w: huffman decode failed (no matching code in %d bits)",
		ErrCorruptedFile, maxAllowedDepth)
}

// readBit reads one bit from the bitstream (MSB-first within a byte —
// matching the writer's LSB-first per-byte packing means the BIT 0 of
// the stored byte is read first).
func (br *bitReader) readBit() (uint, error) {
	if br.dataP >= uint64(len(br.data)) {
		return 0, fmt.Errorf("%w: unexpected EOF in bitstream", ErrCorruptedFile)
	}
	b := br.data[br.dataP]
	// bitWriter packs LSB-first: bit 0 of code goes into the lowest
	// position of the output byte. So we read bit `br.dataBit` of the
	// byte (the lowest-numbered bit first).
	bit := uint((b >> uint(br.dataBit)) & 1)
	br.dataBit++
	if br.dataBit == 8 {
		br.dataBit = 0
		br.dataP++
	}
	return bit, nil
}

// nextWord reads the next word (length code, optional terminator,
// optional raw bytes) and appends it to buf, returning the extended
// slice.
//
// Wire-flow (matches `Getter.Next` at decompress.go:935-1003, with the
// pattern-cover loop elided since v1 only handles the no-pattern fast
// path):
//
//  1. nextPosClean (byte-align then read length code).
//  2. If length == 0 (i.e. length-prefix-code value 1, since the writer
//     stores l+1): align if needed, return empty.
//  3. Else: nextPos to read the terminator (must be 0), byte-align,
//     read `wordLen` raw bytes.
//
// The writer's flush() at the END of each word (compress.go:728-754)
// guarantees the raw bytes start at a byte boundary, so step 3's
// byte-align is the matching read-side handshake.
func (br *bitReader) nextWord(buf []byte) ([]byte, error) {
	lenPlusOne, err := br.nextPosClean()
	if err != nil {
		return nil, err
	}
	if lenPlusOne == 0 {
		// Erigon encodes len+1 always ≥ 1; lenPlusOne == 0 means
		// corrupt input or a bit-stream desync.
		return nil, fmt.Errorf("%w: zero-encoded length", ErrCorruptedFile)
	}
	wordLen := lenPlusOne - 1
	if wordLen == 0 {
		// Empty word: writer emitted just the length code then flush().
		// We need to byte-align here ourselves (the writer's flush()
		// pushed any partial bits to disk, but our dataBit cursor is
		// still mid-byte).
		if br.dataBit > 0 {
			br.dataP++
			br.dataBit = 0
		}
		if buf == nil {
			buf = []byte{}
		}
		return buf, nil
	}
	// Non-empty: read terminator (must be 0), byte-align, read raw bytes.
	term, err := br.nextPos()
	if err != nil {
		return nil, err
	}
	if term != 0 {
		return nil, fmt.Errorf("%w: expected terminator (0), got %d",
			ErrCorruptedFile, term)
	}
	if br.dataBit > 0 {
		br.dataP++
		br.dataBit = 0
	}
	if br.dataP+wordLen > uint64(len(br.data)) {
		return nil, fmt.Errorf("%w: word overruns file (need %d bytes at %d, len=%d)",
			ErrCorruptedFile, wordLen, br.dataP, len(br.data))
	}
	buf = append(buf, br.data[br.dataP:br.dataP+wordLen]...)
	br.dataP += wordLen
	return buf, nil
}
