package ethrex

// ComputeJumpdestBitmap returns the JUMPDEST bitmap ethrex persists alongside a
// bytecode: one bit per bytecode byte, set when that offset holds a JUMPDEST
// (0x5B) that is not part of a PUSH immediate. Bit i lives in bit (i%8) of byte
// (i/8), least-significant bit first, so the bitmap is ceil(len/8) bytes.
//
// Bytecode with no jump destination gets a ZERO-LENGTH bitmap rather than an
// all-zero one: ethrex reads a missing byte as "no jump destination"
// (Code::is_valid_jumpdest), so it never allocates a map for the common
// jumpless case (EOAs, tiny contracts).
//
// Scan rules (ethrex Code::compute_jumpdests, crates/common/types/account.rs):
//   - opcode 0x5B (JUMPDEST): set bit i, advance i by 1.
//   - opcode 0x60..0x7F (PUSH1..PUSH32): advance i by (opcode - 0x5F + 1) to skip
//     the opcode itself plus its immediate bytes.
//   - any other opcode: advance i by 1.
func ComputeJumpdestBitmap(bytecode []byte) []byte {
	bitmap := make([]byte, (len(bytecode)+7)/8)
	found := false
	i := 0
	for i < len(bytecode) {
		op := bytecode[i]
		switch {
		case op == 0x5B:
			bitmap[i/8] |= 1 << (i % 8)
			found = true
			i++
		case op >= 0x60 && op <= 0x7F:
			// PUSH1..PUSH32: skip opcode + immediate bytes.
			i += int(op-0x5F) + 1
		default:
			i++
		}
	}
	if !found {
		return nil
	}
	return bitmap
}

// EncodeCode returns the concatenation of:
//
//  1. RLP(bytecode) — bytecode encoded as an RLP byte string.
//  2. RLP(jumpdestBitmap) — the JUMPDEST bitmap as an RLP byte string.
//
// The two RLP encodings are concatenated directly (not wrapped in an outer list).
// Mirrors ethrex's encode_code (crates/storage/store.rs).
//
// The bitmap replaced an RLP list of u32 JUMPDEST offsets. ethrex still reads
// that older form: decode_jumpdests branches on the RLP item header and rebuilds
// the bitmap from the bytecode when it finds a list.
//
// Golden checks:
//   - EncodeCode(0x60015b00) = 0x8460015b00 04 (JUMPDEST at offset 2)
//   - EncodeCode(0x600160015500) = 0x86600160015500 80 (no JUMPDEST)
//   - EncodeCode(nil) = 0x80 80
func EncodeCode(bytecode []byte) []byte {
	part1 := rlpEncodeBytes(bytecode)
	part2 := rlpEncodeBytes(ComputeJumpdestBitmap(bytecode))
	return append(part1, part2...)
}

// CodeLengthMetadata returns the big-endian u64 encoding of len(bytecode).
// This is the value stored in account_code_metadata.
func CodeLengthMetadata(bytecode []byte) [8]byte {
	n := uint64(len(bytecode))
	var out [8]byte
	out[0] = byte(n >> 56)
	out[1] = byte(n >> 48)
	out[2] = byte(n >> 40)
	out[3] = byte(n >> 32)
	out[4] = byte(n >> 24)
	out[5] = byte(n >> 16)
	out[6] = byte(n >> 8)
	out[7] = byte(n)
	return out
}
