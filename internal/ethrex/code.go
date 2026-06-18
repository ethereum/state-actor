package ethrex

// ComputeJumpTargets scans bytecode and returns the offsets of JUMPDEST (0x5B)
// opcodes that are not inside PUSH data. Mirrors ethrex account.rs:58-79.
//
// Scan rules:
//   - opcode 0x5B (JUMPDEST): append uint32(i), advance i by 1.
//   - opcode 0x60..0x7F (PUSH1..PUSH32): advance i by (opcode - 0x5F + 1) to skip
//     the opcode itself plus its immediate bytes.
//   - any other opcode: advance i by 1.
func ComputeJumpTargets(bytecode []byte) []uint32 {
	var targets []uint32
	i := 0
	for i < len(bytecode) {
		op := bytecode[i]
		switch {
		case op == 0x5B:
			targets = append(targets, uint32(i))
			i++
		case op >= 0x60 && op <= 0x7F:
			// PUSH1..PUSH32: skip opcode + immediate bytes.
			i += int(op-0x5F) + 1
		default:
			i++
		}
	}
	return targets
}

// EncodeCode returns the concatenation of:
//
//  1. RLP(bytecode) — bytecode encoded as an RLP byte string.
//  2. RLP(jumpTargetsList) — jump targets encoded as an RLP list of minimal
//     big-endian integers (one per uint32 target offset).
//
// The two RLP encodings are concatenated directly (not wrapped in an outer list).
// Mirrors ethrex account_codes encoding.
//
// Golden checks:
//   - EncodeCode(0x60015b00) = 0x8460015b00 c1 02 (jumpTargets=[2])
//   - EncodeCode(0x600160015500) = 0x86600160015500 c0 (empty jumpTargets)
//   - EncodeCode(nil) = 0x80 c0
func EncodeCode(bytecode []byte) []byte {
	// Part 1: RLP(bytecode).
	part1 := rlpEncodeBytes(bytecode)

	// Part 2: RLP list of jump target uint32 values.
	targets := ComputeJumpTargets(bytecode)
	var listPayload []byte
	for _, t := range targets {
		listPayload = append(listPayload, rlpEncodeUint32(t)...)
	}
	part2 := rlpEncodeListRaw(listPayload)

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

// rlpEncodeUint32 encodes a uint32 as an RLP integer (minimal big-endian).
func rlpEncodeUint32(n uint32) []byte {
	if n == 0 {
		return []byte{0x80}
	}
	b := minBEBytesU32(n)
	return rlpEncodeBytes(b)
}

// minBEBytesU32 returns the minimal big-endian encoding of a uint32.
func minBEBytesU32(n uint32) []byte {
	var buf [4]byte
	i := 3
	for n > 0 {
		buf[i] = byte(n)
		n >>= 8
		i--
	}
	return buf[i+1:]
}
