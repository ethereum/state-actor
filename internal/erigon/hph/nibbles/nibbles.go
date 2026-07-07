// Vendored from github.com/erigontech/erigon execution/commitment/nibbles/nibbles.go @ 14273f79a6 (production pin).
// Modifications: build tag; R2 strip: CompactToHex/KeybytesToHex/HexToKeybytes (unused post-trim).
//
//go:build cgo_erigon_commitment

// Copyright 2014 The go-ethereum Authors
// (original work)
// Copyright 2024 The Erigon Authors
// (modifications)
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

// Package nibbles provides canonical spec-compliant hex/compact nibble encoding
// helpers as defined in the Ethereum Yellow Paper. Both execution/commitment and
// execution/commitment/trie import from this leaf package, avoiding the import
// cycle that previously forced duplicated implementations.
package nibbles

// Terminator is the hex nibble terminator byte (0x10 = 16).
const Terminator byte = 0x10

// HexToCompact converts a hex nibble sequence to compact (hex-prefix) encoding
// as defined by the Ethereum Yellow Paper.
func HexToCompact(hex []byte) []byte {
	terminator := byte(0)
	if HasTerm(hex) {
		terminator = 1
		hex = hex[:len(hex)-1]
	}
	buf := make([]byte, len(hex)/2+1)
	buf[0] = terminator << 5 // the flag byte
	if len(hex)&1 == 1 {
		buf[0] |= 1 << 4 // odd flag
		buf[0] |= hex[0] // first nibble is contained in the first byte
		hex = hex[1:]
	}
	decodeNibbles(hex, buf[1:])
	return buf
}

// HasTerm returns whether a hex key has the terminator flag.
func HasTerm(s []byte) bool {
	return len(s) > 0 && s[len(s)-1] == Terminator
}

// CommonPrefixLen returns the length of the common prefix of a and b.
func CommonPrefixLen(a, b []byte) int {
	var i, length = 0, len(a)
	if len(b) < length {
		length = len(b)
	}
	for ; i < length; i++ {
		if a[i] != b[i] {
			break
		}
	}
	return i
}

func decodeNibbles(nibbles []byte, bytes []byte) {
	if HasTerm(nibbles) {
		nibbles = nibbles[:len(nibbles)-1]
	}

	nl := len(nibbles)
	for bi, ni := 0, 0; ni < nl; bi, ni = bi+1, ni+2 {
		if ni == nl-1 {
			bytes[bi] = (bytes[bi] &^ 0xf0) | nibbles[ni]<<4
		} else {
			bytes[bi] = nibbles[ni]<<4 | nibbles[ni+1]
		}
	}
}
