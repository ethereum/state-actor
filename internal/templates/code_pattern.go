package templates

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"slices"
	"sync"

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

	// CodePatternMaxSamePreAmsterdam — 24576-byte identical runtime
	// (STOP at 0x00, 24575 JUMPDESTs). Unlike unique patterns, all copies
	// share one code hash (no embedded address). Used by
	// AccountMode.EXISTING_CONTRACT_SAME; CodePatternRuntimeSize = 0.
	CodePatternMaxSamePreAmsterdam = "max_same_pre_amsterdam"

	// CodePatternMaxDiffPreAmsterdam — 24576-byte runtime with embedded
	// address at bytes 0x0C..0x20 (STOP + padding + address + JUMPDESTs).
	// Byte-unique per contract. Used by AccountMode.EXISTING_CONTRACT_DIFF.
	CodePatternMaxDiffPreAmsterdam = "max_diff_pre_amsterdam"

	// CodePatternUniqueJumpdest — size-adjustable variant of the
	// unique-jumpdest layout. The runtime size comes from the optional
	// `code_size:` template parameter (default 24576, max 0x01000000);
	// matches execution-specs `JochemnetPredeployContractInitcode`
	// with the same code_size.
	//
	// The entry jump target is a minimal push, as execution-specs emits
	// it: PUSH1 while `code_size - 1` fits one byte, PUSH2 up to
	// 0x010000, PUSH3 above. The default size lands in the PUSH2 range
	// either way, so it reproduces the pre-Amsterdam initcode
	// byte-for-byte and state already deployed on live testnets keeps
	// its CREATE2 addresses.
	CodePatternUniqueJumpdest = "unique_jumpdest"

	// CodePatternMaxSame — size-adjustable variant of the max-same
	// layout (STOP at byte 0, JUMPDEST elsewhere; all copies byte-
	// identical). `code_size:` defaults to 24576 (byte-identical to
	// max_same_pre_amsterdam there); matches execution-specs
	// `StopJumpdestInitcode(code_size=..., diff=False)`. The layout has
	// no jump target, so no PUSH2/PUSH3 boundary applies — only the
	// MCOPY fill loop and the RETURN length scale with the size.
	CodePatternMaxSame = "max_same"

	// CodePatternMaxDiff — size-adjustable variant of the max-diff
	// layout (STOP + zero padding + own address at 0x0C..0x20, JUMPDEST
	// elsewhere; byte-unique per contract). `code_size:` defaults to
	// 24576 (byte-identical to max_diff_pre_amsterdam there); matches
	// execution-specs `StopJumpdestInitcode(code_size=..., diff=True)`.
	CodePatternMaxDiff = "max_diff"
)

// preAmsterdamMaxCodeSize is the EIP-170 contract-code limit applied
// pre-Amsterdam (also: Fusaka). Amsterdam (EIP-7907) raises this; the
// amsterdam pattern variant will live alongside this one when scheduled.
const preAmsterdamMaxCodeSize = 0x6000 // 24576 bytes.

// maxPatternCodeSize caps the size-adjustable patterns at 16 MiB, three
// bytes of entry jump target. Nothing in the layout breaks above it —
// the entry push simply grows — but no benchmark wants a runtime that
// large, and a cap keeps a mistyped code_size from asking the builder
// for gigabytes of JUMPDEST.
const maxPatternCodeSize = 0x01000000 // 16 MiB

// minUniqueJumpdestCodeSize is the smallest runtime the unique-jumpdest
// layout supports: the embedded-address region ends at byte 0x40 and
// the entry JUMP must land on a JUMPDEST after it.
const minUniqueJumpdestCodeSize = 0x41

// minStopJumpdestCodeSize is the smallest runtime the max-same/max-diff
// layouts support: the max-diff embedded-address region ends at byte
// 0x20 (max-same shares the bound for simplicity).
const minStopJumpdestCodeSize = 0x20

// single source of truth for recognized pattern names.
var knownCodePatterns = []string{
	CodePatternUniqueJumpdestPreAmsterdam,
	CodePatternMaxSamePreAmsterdam,
	CodePatternMaxDiffPreAmsterdam,
	CodePatternUniqueJumpdest,
	CodePatternMaxSame,
	CodePatternMaxDiff,
}

// IsKnownCodePattern reports whether the given string is one of the
// recognized named code patterns. Used at parameter-validate time.
func IsKnownCodePattern(name string) bool {
	return slices.Contains(knownCodePatterns, name)
}

// CodePatternSupportsCodeSize reports whether the named pattern accepts
// the optional `code_size:` template parameter. Fixed-size patterns
// (the *_pre_amsterdam family) are locked to 24576 by their name.
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

// CodePatternRuntimeSize returns runtime size for unique patterns,
// 0 for unknown/shared patterns. Used for resident memory estimation.
// codeSize is the resolved `code_size:` parameter for size-adjustable
// patterns; pass 0 for "parameter omitted" (pattern default applies).
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

// parseCodePatternCodeSize resolves the optional `code_size` parameter
// against the chosen pattern. Returns the effective runtime size for
// size-adjustable patterns (the 24576 default when omitted) and 0 for
// fixed-size patterns; `code_size` on a fixed-size pattern is an error
// rather than silently ignored.
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
	return BuildUniqueJumpdestRuntime(addr, preAmsterdamMaxCodeSize)
}

// BuildUniqueJumpdestRuntime returns the codeSize-byte runtime for one
// derived contract under the size-adjustable unique-jumpdest pattern.
// Same layout as the pre-Amsterdam variant, with the entry jump target
// at codeSize-1 as a minimal push: `PUSH1 target; JUMP` (3 bytes) while
// the target fits one byte, `PUSH2` (4 bytes) up to 0x010000, `PUSH3`
// (5 bytes) above it. All bytes outside the entry and the embedded
// address at 0x2C..0x40 are JUMPDEST.
//
// codeSize must be in [minUniqueJumpdestCodeSize, maxPatternCodeSize];
// parseCodePatternCodeSize enforces this before any builder runs.
func BuildUniqueJumpdestRuntime(addr common.Address, codeSize uint64) []byte {
	out := make([]byte, codeSize)

	// Default-fill with JUMPDEST (0x5B). The doubling-MCOPY loop in the
	// initcode produces the same end state, but a direct fill is faster
	// in Go and equivalent on disk.
	for i := range out {
		out[i] = 0x5B
	}

	// Entry: PUSHn codeSize-1; JUMP.
	copy(out, appendEntryJump(nil, codeSize))

	// Bytes up to 0x02C already JUMPDEST from default fill.

	// Bytes 0x02C..0x03F — the 20-byte address.
	copy(out[0x2C:0x40], addr[:])

	// Bytes 0x040..codeSize-1 already JUMPDEST from default fill (the
	// JUMP target at codeSize-1 is just another byte in the JUMPDEST
	// sea).

	return out
}

// appendEntryJump appends the runtime entry `PUSHn codeSize-1; JUMP`,
// with the target as a minimal push. This mirrors execution-specs
// `Op.JUMP(code_size - 1)`, which is what makes the initcode — and so
// the derived CREATE2 addresses — agree with it at every size. A
// fixed-width PUSH2 was used here before and diverged for every
// codeSize up to 0x100, where the target fits a single byte; the two
// encodings agree from 0x101 up, so the 24576 default and everything
// deployed at it are unaffected.
func appendEntryJump(buf []byte, codeSize uint64) []byte {
	buf = append(buf, pushImmediate(codeSize-1)...)
	return append(buf, 0x56) // JUMP
}

// appendJumpdestFillSeed appends the shared initcode prologue: fills
// memory with JUMPDESTs via PUSH32 + doubling MCOPY until the filled
// span covers codeSize. The unique, max-same, and max-diff patterns all
// build on this prefix; kept centralized to prevent hash drift.
func appendJumpdestFillSeed(buf []byte, codeSize uint64) []byte {
	// PUSH32 (32 × JUMPDEST); PUSH1 0x00; MSTORE.
	buf = append(buf, 0x7F) // PUSH32
	for range 32 {
		buf = append(buf, 0x5B)
	}
	buf = append(buf, 0x60, 0x00) // PUSH1 0
	buf = append(buf, 0x52)       // MSTORE

	// Doubling MCOPY: sizes {32, 64, ...} until the copied span covers
	// codeSize (for the 24576 default: up to 16384, span 32768). Push
	// order: len, src, dst (MCOPY pops with dst on top). Mirrors the
	// Python `range(5, (code_size - 1).bit_length())` loop.
	for s := uint(5); s < uint(bits.Len64(codeSize-1)); s++ {
		size := uint64(1) << s
		buf = append(buf, pushImmediate(size)...) // len
		buf = append(buf, 0x60, 0x00)             // PUSH1 0 (src)
		buf = append(buf, pushImmediate(size)...) // dst
		buf = append(buf, 0x5E)                   // MCOPY
	}
	return buf
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
	return BuildUniqueJumpdestInitcode(preAmsterdamMaxCodeSize)
}

// BuildUniqueJumpdestInitcode returns the initcode that — if run —
// would deploy the codeSize-byte unique-jumpdest runtime. Same shape as
// the pre-Amsterdam variant with two size-dependent spots, both of them
// minimal pushes as Python emits them: the entry jump target in step 3
// (see appendEntryJump) and the RETURN length in step 5.
// For the 24576 default the output is byte-identical to
// BuildUniqueJumpdestInitcodePreAmsterdam. Vendored from
// execution-specs `JochemnetPredeployContractInitcode`; only the
// keccak256 hash is used (the initcode never executes).
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

	// 5. PUSH codeSize; PUSH1 0x00; RETURN.
	buf = append(buf, pushImmediate(codeSize)...) // length
	buf = append(buf, 0x60, 0x00)                 // PUSH1 0 (offset)
	buf = append(buf, 0xF3)                       // RETURN

	return buf
}

// maxSameRuntimePreAmsterdam is the shared runtime for max-same pattern.
// All derived contracts alias this read-only slice (byte-identical).
var maxSameRuntimePreAmsterdam = BuildMaxSameRuntime(preAmsterdamMaxCodeSize)

// maxSameRuntimeCache memoizes the shared max-same runtime per code
// size: the runtime is byte-identical across all derived contracts, so
// every contract of a given size aliases one slice instead of holding
// its own copy. Keyed by codeSize; values are []byte.
var maxSameRuntimeCache sync.Map

// maxSameRuntimeFor returns the shared max-same runtime for codeSize,
// building and caching it on first use.
func maxSameRuntimeFor(codeSize uint64) []byte {
	if v, ok := maxSameRuntimeCache.Load(codeSize); ok {
		return v.([]byte)
	}
	rt, _ := maxSameRuntimeCache.LoadOrStore(codeSize, BuildMaxSameRuntime(codeSize))
	return rt.([]byte)
}

// BuildMaxSameRuntime returns the codeSize-byte runtime: a STOP (0x00)
// at offset 0 so a call halts immediately, JUMPDEST (0x5B) for every
// other byte. Matches the deployed code of execution-specs
// StopJumpdestInitcode(code_size=codeSize, diff=False).
func BuildMaxSameRuntime(codeSize uint64) []byte {
	out := make([]byte, codeSize)
	for i := range out {
		out[i] = 0x5B // JUMPDEST
	}
	out[0] = 0x00 // STOP — a call to the contract halts at byte 0.
	return out
}

// BuildMaxSameInitcodePreAmsterdam returns initcode that deploys a
// STOP + JUMPDEST-sea runtime at the fixed pre-Amsterdam 24576 size.
func BuildMaxSameInitcodePreAmsterdam() []byte {
	return BuildMaxSameInitcode(preAmsterdamMaxCodeSize)
}

// BuildMaxSameInitcode returns initcode that deploys the codeSize-byte
// STOP + JUMPDEST-sea runtime. Vendored from execution-specs
// StopJumpdestInitcode(diff=False) to match bench test CREATE2
// derivation; only the hash is used (initcode never executes). The
// layout has no jump target, so no PUSH2/PUSH3 boundary applies — only
// the fill loop and the RETURN length (minimal push, matching Python's
// Op.RETURN encoding) scale with the size.
//
// Steps: fill mem with JUMPDESTs, overwrite mem[0] with STOP, return
// codeSize bytes.
func BuildMaxSameInitcode(codeSize uint64) []byte {
	// 1+2. Seed memory covering codeSize with JUMPDEST (shared prologue).
	buf := appendJumpdestFillSeed(nil, codeSize)

	// 3. MSTORE8(0, 0): MSTORE8 pops (offset, value) with offset on top,
	// so push value first, then offset.
	buf = append(buf, 0x60, 0x00) // PUSH1 0 (value)
	buf = append(buf, 0x60, 0x00) // PUSH1 0 (offset)
	buf = append(buf, 0x53)       // MSTORE8

	// 4. PUSH codeSize; PUSH1 0x00; RETURN.
	buf = append(buf, pushImmediate(codeSize)...) // length
	buf = append(buf, 0x60, 0x00)                 // PUSH1 0 (offset)
	buf = append(buf, 0xF3)                       // RETURN

	return buf
}

// BuildMaxDiffRuntimePreAmsterdam returns the 24576-byte runtime with
// embedded address (STOP + padding + address + JUMPDESTs at 0x0C..0x20).
// Byte-unique per contract. Matches UniqueMaxContractInitcode(diff=True).
func BuildMaxDiffRuntimePreAmsterdam(addr common.Address) []byte {
	return BuildMaxDiffRuntime(addr, preAmsterdamMaxCodeSize)
}

// BuildMaxDiffRuntime returns the codeSize-byte runtime with embedded
// address (STOP + padding + address at 0x0C..0x20, JUMPDESTs
// elsewhere). Byte-unique per contract. Matches execution-specs
// StopJumpdestInitcode(code_size=codeSize, diff=True).
func BuildMaxDiffRuntime(addr common.Address, codeSize uint64) []byte {
	out := make([]byte, codeSize)
	for i := range out {
		out[i] = 0x5B // JUMPDEST
	}
	// Bytes 0x00..0x0C — STOP at byte 0 plus 11 zero padding bytes
	// (MSTORE(0, ADDRESS) writes the address right-aligned in 32 bytes).
	for i := 0; i < 0x0C; i++ {
		out[i] = 0x00
	}
	// Bytes 0x0C..0x20 — the 20-byte address.
	copy(out[0x0C:0x20], addr[:])
	// Bytes 0x20..codeSize stay JUMPDEST from the fill.
	return out
}

// BuildMaxDiffInitcodePreAmsterdam returns initcode with embedded
// ADDRESS at the fixed pre-Amsterdam 24576 size.
func BuildMaxDiffInitcodePreAmsterdam() []byte {
	return BuildMaxDiffInitcode(preAmsterdamMaxCodeSize)
}

// BuildMaxDiffInitcode returns initcode with embedded ADDRESS deploying
// the codeSize-byte max-diff runtime. Vendored from execution-specs
// StopJumpdestInitcode(diff=True) for CREATE2; only the hash is used
// (never executes). Size-scaling as in BuildMaxSameInitcode.
func BuildMaxDiffInitcode(codeSize uint64) []byte {
	// 1+2. Seed memory covering codeSize with JUMPDEST (shared prologue).
	buf := appendJumpdestFillSeed(nil, codeSize)

	// 3. MSTORE(0, ADDRESS): MSTORE pops (offset, value) with offset on
	// top, so push value (ADDRESS) first, then offset.
	buf = append(buf, 0x30)       // ADDRESS (value)
	buf = append(buf, 0x60, 0x00) // PUSH1 0 (offset)
	buf = append(buf, 0x52)       // MSTORE

	// 4. PUSH codeSize; PUSH1 0x00; RETURN.
	buf = append(buf, pushImmediate(codeSize)...) // length
	buf = append(buf, 0x60, 0x00)                 // PUSH1 0 (offset)
	buf = append(buf, 0xF3)                       // RETURN

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
// stays in one place. Unknown names yield an error. codeSize is the
// resolved `code_size:` parameter (parseCodePatternCodeSize output);
// fixed-size patterns receive 0 and ignore it.
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

// codePatternInitcodeFor returns the (per-pattern) shared initcode used
// for CREATE2 address derivation. The initcode is the SAME for every
// derived address — only the deployed runtime is per-address-unique.
// create_preimage_deploys ignores initcode entirely (CREATE address
// derivation doesn't hash the initcode). codeSize as in
// codePatternRuntimeFor.
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
