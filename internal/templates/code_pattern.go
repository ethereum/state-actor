package templates

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// Code-pattern named values. Each constant identifies one named runtime/
// initcode generator the create2_deploys and create_preimage_deploys
// templates can opt into via `code_pattern: <name>`. The fork suffix is
// explicit because the bytecode layout depends on EIP-170's MAX_CODE_SIZE,
// which Amsterdam (EIP-7907) raises.
const (
	// CodePatternUniqueJumpdestPreAmsterdam — 24576-byte runtime per
	// derived contract, layout matching execution-specs
	// `tests/benchmark/stateful/bloatnet/test_transaction_types.py::
	// build_unique_contract_initcode`. Used in the
	// `diff_to_unique_code_jumpdest_contract` benchmark case.
	//
	// Each derived contract is byte-unique because its own 20-byte address
	// is embedded at bytes 0x2C..0x40 of the runtime. The runtime entry
	// at offset 0 is `PUSH2 0x5FFF; JUMP` — control lands at the JUMPDEST
	// byte at offset 0x5FFF, then drops off the code (no STOP needed —
	// the EVM treats end-of-code as a clean exit).
	CodePatternUniqueJumpdestPreAmsterdam = "unique_jumpdest_pre_amsterdam"
)

// preAmsterdamMaxCodeSize is the EIP-170 contract-code limit applied
// pre-Amsterdam (also: Fusaka). Amsterdam (EIP-7907) raises this; the
// amsterdam pattern variant will live alongside this one when scheduled.
const preAmsterdamMaxCodeSize = 0x6000 // 24576 bytes.

// IsKnownCodePattern reports whether the given string is one of the
// recognized named code patterns. Used at parameter-validate time.
func IsKnownCodePattern(name string) bool {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return true
	}
	return false
}

// CodePatternRuntimeSize returns the per-derived-contract runtime size
// in bytes for a known pattern name; 0 for unknown names. Used to
// estimate resident memory: pattern runtimes are byte-unique per
// derived address, so the full count × size set stays reachable (via
// generator.Config.GenesisCode) for the entire run.
func CodePatternRuntimeSize(name string) uint64 {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return preAmsterdamMaxCodeSize
	}
	return 0
}

// patternResidentCodeCapBytes hard-caps the estimated unique-runtime
// residency of a single pattern-mode entity. 64 GiB clears the
// production 1.5M-contract scale (≈34.3 GiB) with headroom but cannot
// fit beside streamsort/trie overheads even on a 128 GB build host.
// True per-entity code streaming is a tracked follow-up; until then the
// cap turns an un-runnable build into a validate-time error.
const patternResidentCodeCapBytes = uint64(64) << 30 // 64 GiB

// BuildUniqueJumpdestRuntimePreAmsterdam returns the 24576-byte runtime
// for one derived contract under the unique-jumpdest pattern.
//
// Layout (matches execution-specs build_unique_contract_initcode):
//
//	offset    size         contents
//	------    ----         --------
//	0x0000      4          PUSH2 0x5FFF; JUMP
//	0x0004     40          JUMPDEST padding (0x5B × 40)
//	0x002C     20          embedded contract address (20 bytes)
//	0x0040  24512          JUMPDEST filler (0x5B × 24512)
//	0x5FFF      1          JUMPDEST (where the JUMP at offset 0 lands)
//
// All bytes outside the entry instructions and the embedded address are
// JUMPDEST (0x5B), so the JUMP target at 0x5FFF is valid and any
// off-by-one access (e.g. a future EXTCODECOPY at a random offset) hits
// well-defined behavior instead of an invalid jump.
func BuildUniqueJumpdestRuntimePreAmsterdam(addr common.Address) []byte {
	out := make([]byte, preAmsterdamMaxCodeSize)

	// Default-fill with JUMPDEST (0x5B). The doubling-MCOPY loop in the
	// initcode produces the same end state, but a direct fill is faster
	// in Go and equivalent on disk.
	for i := range out {
		out[i] = 0x5B
	}

	// Bytes 0x000..0x003 — entry: PUSH2 0x5FFF; JUMP.
	out[0] = 0x61 // PUSH2
	out[1] = 0x5F
	out[2] = 0xFF
	out[3] = 0x56 // JUMP

	// Bytes 0x004..0x02B already JUMPDEST from default fill.

	// Bytes 0x02C..0x03F — the 20-byte address.
	copy(out[0x2C:0x40], addr[:])

	// Bytes 0x040..0x5FFF already JUMPDEST from default fill (the JUMP
	// target at 0x5FFF is just another byte in the JUMPDEST sea).

	return out
}

// BuildUniqueJumpdestInitcodePreAmsterdam returns the initcode that —
// if run — would deploy the unique-jumpdest runtime above. State-actor
// never executes this initcode; only its keccak256 hash is used by
// CREATE2 address derivation. Vendored from execution-specs's
// `build_unique_contract_initcode` (Python) so the derived addresses
// match what the bench tests compute via `Create2PreimageLayout`.
//
// Initcode shape:
//  1. PUSH32 (32 JUMPDEST bytes); PUSH1 0x00; MSTORE — seed mem[0:32].
//  2. Doubling MCOPY for sizes 32, 64, 128, ..., 16384 — fill
//     mem[0:32768] with JUMPDEST.
//  3. PUSH32 entry; PUSH1 0x00; MSTORE — overwrite mem[0:32] with
//     `PUSH2 0x5FFF; JUMP` followed by JUMPDEST padding.
//  4. PUSH32 addr_slot; ADDRESS; OR; PUSH1 0x20; MSTORE — overwrite
//     mem[0x20:0x40] so bytes 0x2C..0x40 hold the contract's address.
//  5. PUSH2 0x6000; PUSH1 0x00; RETURN — emit the first 0x6000 bytes
//     of memory as the runtime.
func BuildUniqueJumpdestInitcodePreAmsterdam() []byte {
	var buf []byte

	// 1. PUSH32 (32 × JUMPDEST); PUSH1 0x00; MSTORE.
	buf = append(buf, 0x7F) // PUSH32
	for range 32 {
		buf = append(buf, 0x5B)
	}
	buf = append(buf, 0x60, 0x00) // PUSH1 0
	buf = append(buf, 0x52)       // MSTORE

	// 2. Doubling MCOPY: size in {32, 64, 128, 256, 512, 1024, 2048,
	//    4096, 8192, 16384}. MCOPY pops (dst, src, len) with dst on top,
	//    so we push len first, then src, then dst.
	for s := uint(5); s < 15; s++ {
		size := uint64(1) << s
		buf = append(buf, pushImmediate(size)...) // len
		buf = append(buf, 0x60, 0x00)             // PUSH1 0 (src)
		buf = append(buf, pushImmediate(size)...) // dst
		buf = append(buf, 0x5E)                   // MCOPY
	}

	// 3. PUSH32 entry; PUSH1 0x00; MSTORE.
	// entry = PUSH2 0x5FFF; JUMP (4 bytes) + JUMPDEST × 28.
	buf = append(buf, 0x7F) // PUSH32
	buf = append(buf, 0x61, 0x5F, 0xFF, 0x56)
	for range 28 {
		buf = append(buf, 0x5B)
	}
	buf = append(buf, 0x60, 0x00) // PUSH1 0
	buf = append(buf, 0x52)       // MSTORE

	// 4. PUSH32 addr_slot; ADDRESS; OR; PUSH1 0x20; MSTORE.
	// addr_slot = JUMPDEST × 12 + STOP × 20 = 0x5B...5B 0x00...00.
	// The OR mixes the 20-byte ADDRESS into the low 20 bytes of the
	// template (whose low 20 bytes are zero); high 12 bytes carry the
	// JUMPDEST padding through unchanged. Order matters: push template
	// FIRST so when ADDRESS pushes (top = address, below = template),
	// OR consumes both and yields template | (0-padded-address) = the
	// masked template.
	buf = append(buf, 0x7F) // PUSH32
	for range 12 {
		buf = append(buf, 0x5B)
	}
	for range 20 {
		buf = append(buf, 0x00)
	}
	buf = append(buf, 0x30)       // ADDRESS
	buf = append(buf, 0x17)       // OR
	buf = append(buf, 0x60, 0x20) // PUSH1 0x20 (offset)
	buf = append(buf, 0x52)       // MSTORE

	// 5. PUSH2 0x6000; PUSH1 0x00; RETURN.
	buf = append(buf, 0x61, 0x60, 0x00) // PUSH2 0x6000 (length)
	buf = append(buf, 0x60, 0x00)       // PUSH1 0 (offset)
	buf = append(buf, 0xF3)             // RETURN

	return buf
}

// pushImmediate emits the smallest PUSHN + immediate bytes that pushes
// `v` onto the EVM stack. Used by the initcode builder to keep the
// emitted blob compact (32 → PUSH1, 256 → PUSH2, …, up to PUSH8 since
// the initcode only ever pushes sizes up to 16384).
func pushImmediate(v uint64) []byte {
	if v == 0 {
		return []byte{0x5F} // PUSH0
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	start := 0
	for start < 7 && buf[start] == 0 {
		start++
	}
	raw := buf[start:]
	op := byte(0x60 + len(raw) - 1) // PUSH1..PUSH8
	out := make([]byte, 0, 1+len(raw))
	out = append(out, op)
	out = append(out, raw...)
	return out
}

// codePatternRuntimeFor returns the per-address runtime for the named
// pattern. Templates dispatch through this so the named-pattern logic
// stays in one place. Unknown names yield an error.
func codePatternRuntimeFor(name string, addr common.Address) ([]byte, error) {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return BuildUniqueJumpdestRuntimePreAmsterdam(addr), nil
	}
	return nil, fmt.Errorf("unknown code_pattern %q", name)
}

// codePatternInitcodeFor returns the (per-pattern) shared initcode used
// for CREATE2 address derivation. The initcode is the SAME for every
// derived address — only the deployed runtime is per-address-unique.
// create_preimage_deploys ignores initcode entirely (CREATE address
// derivation doesn't hash the initcode).
func codePatternInitcodeFor(name string) ([]byte, error) {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return BuildUniqueJumpdestInitcodePreAmsterdam(), nil
	}
	return nil, fmt.Errorf("unknown code_pattern %q", name)
}
