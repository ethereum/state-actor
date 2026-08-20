package templates

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

// Named runtime/initcode generators the create2_deploys and
// create_preimage_deploys templates opt into via `code_pattern: <name>`.
// The fork suffix is explicit because the layout depends on EIP-170's
// MAX_CODE_SIZE, which EIP-7954 raises at Amsterdam.
const (
	// 24576-byte runtime, byte-unique per contract: its own address is
	// embedded at 0x2C..0x40. Entry `PUSH2 0x5FFF; JUMP` lands on the
	// JUMPDEST at 0x5FFF and runs off the end, which halts cleanly.
	// Backs the `diff_to_unique_code_jumpdest_contract` benchmark case.
	CodePatternUniqueJumpdestPreAmsterdam = "unique_jumpdest_pre_amsterdam"

	// 24576-byte runtime, identical across copies (STOP at 0x00, then
	// JUMPDESTs), so they share one code hash. AccountMode
	// EXISTING_CONTRACT_SAME; CodePatternRuntimeSize = 0.
	CodePatternMaxSamePreAmsterdam = "max_same_pre_amsterdam"

	// As above but with the address embedded at 0x0C..0x20, so
	// byte-unique per contract. AccountMode EXISTING_CONTRACT_DIFF.
	CodePatternMaxDiffPreAmsterdam = "max_diff_pre_amsterdam"

	// The three above with the runtime size taken from the optional
	// `code_size:` parameter instead of hard-coded, matching
	// execution-specs `JochemnetPredeployContractInitcode(code_size=...)`
	// and `StopJumpdestInitcode(code_size=..., diff=...)`. At the 24576
	// default each is byte-identical to its *_pre_amsterdam counterpart,
	// so state already deployed on live testnets keeps its addresses.
	CodePatternUniqueJumpdest = "unique_jumpdest"
	CodePatternMaxSame        = "max_same"
	CodePatternMaxDiff        = "max_diff"
)

// preAmsterdamMaxCodeSize is the EIP-170 limit, and the default for the
// size-adjustable patterns.
const preAmsterdamMaxCodeSize = 0x6000 // 24576 bytes.

// maxPatternCodeSize caps the size-adjustable patterns. Nothing in the
// layout breaks above it, but a cap keeps a mistyped code_size from
// asking the builder for gigabytes of JUMPDEST.
const maxPatternCodeSize = 0x01000000 // 16 MiB

// minUniqueJumpdestCodeSize: the entry JUMP targets codeSize-1, and
// jumpdest analysis decodes the address at 0x2C..0x40 as opcodes, so an
// address byte in the PUSH range turns bytes behind it into push data
// and the jump halts exceptionally. A PUSH32 in the last address byte
// (0x3F) reaches 0x5F; past that every byte is JUMPDEST and the scan
// stays aligned, so a target at 0x60 or beyond is always valid. At the
// 0x41 this once allowed, 72% of derived addresses were uncallable.
const minUniqueJumpdestCodeSize = 0x61

// minStopJumpdestCodeSize: the max-diff address region ends at 0x20
// (max-same shares the bound for simplicity).
const minStopJumpdestCodeSize = 0x20

var knownCodePatterns = []string{
	CodePatternUniqueJumpdestPreAmsterdam,
	CodePatternMaxSamePreAmsterdam,
	CodePatternMaxDiffPreAmsterdam,
	CodePatternUniqueJumpdest,
	CodePatternMaxSame,
	CodePatternMaxDiff,
}

// IsKnownCodePattern reports whether name is a recognized pattern.
func IsKnownCodePattern(name string) bool {
	return slices.Contains(knownCodePatterns, name)
}

// CodePatternSupportsCodeSize reports whether name accepts `code_size:`.
// The *_pre_amsterdam family is locked to 24576 by its name.
func CodePatternSupportsCodeSize(name string) bool {
	switch name {
	case CodePatternUniqueJumpdest, CodePatternMaxSame, CodePatternMaxDiff:
		return true
	}
	return false
}

// codePatternMinCodeSize returns the smallest code_size the named
// size-adjustable pattern's layout supports.
func codePatternMinCodeSize(name string) uint64 {
	if name == CodePatternUniqueJumpdest {
		return minUniqueJumpdestCodeSize
	}
	return minStopJumpdestCodeSize
}

// CodePatternRuntimeSize returns the per-contract runtime size for
// resident-memory estimation: 0 for shared or unknown patterns, since
// their copies alias one slice. codeSize is the resolved `code_size:`,
// or 0 when the parameter was omitted.
func CodePatternRuntimeSize(name string, codeSize uint64) uint64 {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return preAmsterdamMaxCodeSize
	case CodePatternMaxDiffPreAmsterdam:
		return preAmsterdamMaxCodeSize
	case CodePatternMaxSamePreAmsterdam:
		return 0
	case CodePatternUniqueJumpdest, CodePatternMaxDiff:
		if codeSize == 0 {
			return preAmsterdamMaxCodeSize
		}
		return codeSize
	case CodePatternMaxSame:
		// Shared runtime: all derived contracts alias one slice.
		return 0
	}
	return 0
}

// parseCodePatternCodeSize resolves `code_size` against the pattern:
// the effective size for size-adjustable patterns, 0 for fixed-size
// ones. `code_size` on a fixed-size pattern is an error rather than
// silently ignored.
func parseCodePatternCodeSize(params map[string]any, patternName string) (uint64, error) {
	v, has := params["code_size"]
	if !has {
		if CodePatternSupportsCodeSize(patternName) {
			return preAmsterdamMaxCodeSize, nil
		}
		return 0, nil
	}
	if !CodePatternSupportsCodeSize(patternName) {
		return 0, fmt.Errorf("`code_size` is not valid with fixed-size code_pattern %q (the size-adjustable patterns are: %s, %s, %s)",
			patternName, CodePatternUniqueJumpdest, CodePatternMaxSame, CodePatternMaxDiff)
	}
	cs, err := ParseUint64Param(v, "code_size")
	if err != nil {
		return 0, err
	}
	if minSize := codePatternMinCodeSize(patternName); cs < minSize || cs > maxPatternCodeSize {
		return 0, fmt.Errorf("code_size=%#x out of range [%#x, %#x] for code_pattern %q",
			cs, minSize, maxPatternCodeSize, patternName)
	}
	return cs, nil
}

// patternResidentCodeCapBytes caps one entity's estimated unique-runtime
// residency. 64 GiB clears the 1.5M-contract scale (≈34.3 GiB) but
// cannot fit beside streamsort/trie overheads on a 128 GB host. Until
// per-entity code streaming lands, this turns an un-runnable build into
// a validate-time error.
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
// Everything outside the entry and the address is JUMPDEST, so an
// off-by-one access lands on well-defined behavior.
func BuildUniqueJumpdestRuntimePreAmsterdam(addr common.Address) []byte {
	return BuildUniqueJumpdestRuntime(addr, preAmsterdamMaxCodeSize)
}

// BuildUniqueJumpdestRuntime is the layout above at codeSize, with the
// entry jump target as a minimal push. codeSize must be in
// [minUniqueJumpdestCodeSize, maxPatternCodeSize], which
// parseCodePatternCodeSize enforces before any builder runs.
func BuildUniqueJumpdestRuntime(addr common.Address, codeSize uint64) []byte {
	out := make([]byte, codeSize)

	// The initcode's MCOPY loop reaches the same end state; a direct
	// fill is faster in Go and identical on disk.
	for i := range out {
		out[i] = 0x5B // JUMPDEST
	}
	copy(out, appendEntryJump(nil, codeSize))
	copy(out[0x2C:0x40], addr[:])

	return out
}

// appendEntryJump appends `PUSHn codeSize-1; JUMP` with the target as a
// minimal push, mirroring execution-specs `Op.JUMP(code_size - 1)`. This
// was a fixed-width PUSH2 before, which diverged from EEST for every
// codeSize up to 0x100 where the target fits one byte.
func appendEntryJump(buf []byte, codeSize uint64) []byte {
	buf = append(buf, pushImmediate(codeSize-1)...)
	return append(buf, 0x56) // JUMP
}

// appendJumpdestFillSeed appends the initcode prologue shared by all
// three layouts: fill memory with JUMPDESTs via PUSH32 + doubling MCOPY
// until the span covers codeSize. Centralized to prevent hash drift.
func appendJumpdestFillSeed(buf []byte, codeSize uint64) []byte {
	// PUSH32 (32 × JUMPDEST); PUSH1 0x00; MSTORE.
	buf = append(buf, 0x7F) // PUSH32
	for range 32 {
		buf = append(buf, 0x5B)
	}
	buf = append(buf, 0x60, 0x00) // PUSH1 0
	buf = append(buf, 0x52)       // MSTORE

	// Push order is len, src, dst — MCOPY pops with dst on top. Mirrors
	// Python's `range(5, (code_size - 1).bit_length())`.
	for s := uint(5); s < uint(bits.Len64(codeSize-1)); s++ {
		size := uint64(1) << s
		buf = append(buf, pushImmediate(size)...) // len
		buf = append(buf, 0x60, 0x00)             // PUSH1 0 (src)
		buf = append(buf, pushImmediate(size)...) // dst
		buf = append(buf, 0x5E)                   // MCOPY
	}
	return buf
}

// BuildUniqueJumpdestInitcodePreAmsterdam returns the initcode that
// would deploy the runtime above. State-actor never runs it; only its
// keccak256 feeds CREATE2 address derivation. Vendored from
// execution-specs so the derived addresses agree with the bench tests.
//
// Shape: seed memory with JUMPDESTs (steps 1-2 in
// appendJumpdestFillSeed), overwrite mem[0:32] with the entry, OR the
// contract's own ADDRESS into mem[0x20:0x40], RETURN codeSize bytes.
func BuildUniqueJumpdestInitcodePreAmsterdam() []byte {
	return BuildUniqueJumpdestInitcode(preAmsterdamMaxCodeSize)
}

// BuildUniqueJumpdestInitcode is the above at codeSize. Two spots
// scale, both minimal pushes as Python emits them: the entry jump target
// and the RETURN length. Byte-identical to the pre-Amsterdam builder at
// the 24576 default.
func BuildUniqueJumpdestInitcode(codeSize uint64) []byte {
	// Step 1 and 2: seed memory covering codeSize with JUMPDEST
	// (shared prologue).
	buf := appendJumpdestFillSeed(nil, codeSize)

	// 3. PUSH32 entry; PUSH1 0x00; MSTORE.
	// entry = PUSH2/PUSH3 codeSize-1; JUMP + JUMPDEST padding to 32.
	buf = append(buf, 0x7F) // PUSH32
	entry := appendEntryJump(nil, codeSize)
	buf = append(buf, entry...)
	for range 32 - len(entry) {
		buf = append(buf, 0x5B)
	}
	buf = append(buf, 0x60, 0x00) // PUSH1 0
	buf = append(buf, 0x52)       // MSTORE

	// OR the ADDRESS into a `JUMPDEST × 12 + zero × 20` template, so the
	// padding survives and the low 20 bytes become the address. The
	// template is pushed first: OR sees address on top, template below.
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

	buf = append(buf, pushImmediate(codeSize)...) // RETURN length
	buf = append(buf, 0x60, 0x00)                 // PUSH1 0 (offset)
	buf = append(buf, 0xF3)                       // RETURN

	return buf
}

// The max-same runtime is byte-identical across contracts, so every
// contract of a size aliases one slice rather than holding a copy.
var maxSameRuntimePreAmsterdam = BuildMaxSameRuntime(preAmsterdamMaxCodeSize)

var maxSameRuntimeCache sync.Map // codeSize -> []byte

// maxSameRuntimeFor returns the shared runtime for codeSize, building it
// on first use.
func maxSameRuntimeFor(codeSize uint64) []byte {
	if v, ok := maxSameRuntimeCache.Load(codeSize); ok {
		return v.([]byte)
	}
	rt, _ := maxSameRuntimeCache.LoadOrStore(codeSize, BuildMaxSameRuntime(codeSize))
	return rt.([]byte)
}

// BuildMaxSameRuntime returns STOP at byte 0 so a call halts
// immediately, JUMPDEST for the rest. Matches the code deployed by
// execution-specs StopJumpdestInitcode(code_size=..., diff=False).
func BuildMaxSameRuntime(codeSize uint64) []byte {
	out := make([]byte, codeSize)
	for i := range out {
		out[i] = 0x5B // JUMPDEST
	}
	out[0] = 0x00 // STOP
	return out
}

// BuildMaxSameInitcodePreAmsterdam is BuildMaxSameInitcode at 24576.
func BuildMaxSameInitcodePreAmsterdam() []byte {
	return BuildMaxSameInitcode(preAmsterdamMaxCodeSize)
}

// BuildMaxSameInitcode deploys the runtime above: fill memory with
// JUMPDESTs, overwrite mem[0] with STOP, RETURN codeSize bytes. Vendored
// from execution-specs StopJumpdestInitcode(diff=False); only the hash
// is used. No jump target here, so only the fill loop and the RETURN
// length scale with the size.
func BuildMaxSameInitcode(codeSize uint64) []byte {
	// 1+2. Seed memory covering codeSize with JUMPDEST (shared prologue).
	buf := appendJumpdestFillSeed(nil, codeSize)

	// MSTORE8 pops offset then value, so push value first.
	buf = append(buf, 0x60, 0x00) // PUSH1 0 (value)
	buf = append(buf, 0x60, 0x00) // PUSH1 0 (offset)
	buf = append(buf, 0x53)       // MSTORE8

	buf = append(buf, pushImmediate(codeSize)...) // RETURN length
	buf = append(buf, 0x60, 0x00)                 // PUSH1 0 (offset)
	buf = append(buf, 0xF3)                       // RETURN

	return buf
}

// BuildMaxDiffRuntimePreAmsterdam is BuildMaxDiffRuntime at 24576.
func BuildMaxDiffRuntimePreAmsterdam(addr common.Address) []byte {
	return BuildMaxDiffRuntime(addr, preAmsterdamMaxCodeSize)
}

// BuildMaxDiffRuntime is BuildMaxSameRuntime with the contract's own
// address at 0x0C..0x20, making each copy byte-unique. Matches
// execution-specs StopJumpdestInitcode(code_size=..., diff=True).
func BuildMaxDiffRuntime(addr common.Address, codeSize uint64) []byte {
	out := make([]byte, codeSize)
	for i := range out {
		out[i] = 0x5B // JUMPDEST
	}
	// STOP plus 11 zero bytes: MSTORE(0, ADDRESS) writes the address
	// right-aligned in a 32-byte word.
	for i := range 0x0C {
		out[i] = 0x00
	}
	copy(out[0x0C:0x20], addr[:])
	return out
}

// BuildMaxDiffInitcodePreAmsterdam is BuildMaxDiffInitcode at 24576.
func BuildMaxDiffInitcodePreAmsterdam() []byte {
	return BuildMaxDiffInitcode(preAmsterdamMaxCodeSize)
}

// BuildMaxDiffInitcode is BuildMaxSameInitcode with MSTORE(0, ADDRESS)
// in place of the STOP write. Vendored from execution-specs
// StopJumpdestInitcode(diff=True).
func BuildMaxDiffInitcode(codeSize uint64) []byte {
	buf := appendJumpdestFillSeed(nil, codeSize)

	// MSTORE pops offset then value, so push ADDRESS first.
	buf = append(buf, 0x30)       // ADDRESS (value)
	buf = append(buf, 0x60, 0x00) // PUSH1 0 (offset)
	buf = append(buf, 0x52)       // MSTORE

	buf = append(buf, pushImmediate(codeSize)...) // RETURN length
	buf = append(buf, 0x60, 0x00)                 // PUSH1 0 (offset)
	buf = append(buf, 0xF3)                       // RETURN

	return buf
}

// pushImmediate emits the smallest PUSHn that pushes v, which is how
// execution-specs encodes the same values.
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

// codePatternRuntimeFor returns the per-address runtime for a pattern.
// codeSize is parseCodePatternCodeSize's output; fixed-size patterns
// receive 0 and ignore it.
func codePatternRuntimeFor(name string, codeSize uint64, addr common.Address) ([]byte, error) {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return BuildUniqueJumpdestRuntimePreAmsterdam(addr), nil
	case CodePatternMaxSamePreAmsterdam:
		return maxSameRuntimePreAmsterdam, nil
	case CodePatternMaxDiffPreAmsterdam:
		return BuildMaxDiffRuntimePreAmsterdam(addr), nil
	case CodePatternUniqueJumpdest:
		return BuildUniqueJumpdestRuntime(addr, codeSize), nil
	case CodePatternMaxSame:
		return maxSameRuntimeFor(codeSize), nil
	case CodePatternMaxDiff:
		return BuildMaxDiffRuntime(addr, codeSize), nil
	}
	return nil, fmt.Errorf("unknown code_pattern %q", name)
}

// codePatternInitcodeFor returns the initcode CREATE2 derivation hashes.
// It is the same for every derived address — only the deployed runtime
// varies. create_preimage_deploys ignores it, since CREATE addresses do
// not hash the initcode.
func codePatternInitcodeFor(name string, codeSize uint64) ([]byte, error) {
	switch name {
	case CodePatternUniqueJumpdestPreAmsterdam:
		return BuildUniqueJumpdestInitcodePreAmsterdam(), nil
	case CodePatternMaxSamePreAmsterdam:
		return BuildMaxSameInitcodePreAmsterdam(), nil
	case CodePatternMaxDiffPreAmsterdam:
		return BuildMaxDiffInitcodePreAmsterdam(), nil
	case CodePatternUniqueJumpdest:
		return BuildUniqueJumpdestInitcode(codeSize), nil
	case CodePatternMaxSame:
		return BuildMaxSameInitcode(codeSize), nil
	case CodePatternMaxDiff:
		return BuildMaxDiffInitcode(codeSize), nil
	}
	return nil, fmt.Errorf("unknown code_pattern %q", name)
}
