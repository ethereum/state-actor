package rlp

import (
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// EncodeHeader returns the RLP encoding of a block header in the byte
// shape Nethermind's HeaderDecoder expects.
//
// Implementation strategy: wrap go-ethereum's `*types.Header`. Both
// clients follow standard Ethereum RLP (EIP-1559, Shanghai, Cancun,
// Prague), and go-ethereum's Header struct already declares the
// EIP-conditional fields with `rlp:"optional"` tags — the same
// "trailing-optional cascade" Nethermind enforces in its
// `HeaderDecoder.cs:requiredItems[]` backward-propagation loop. If field N
// is non-nil, all earlier optionals are emitted (with zero defaults if
// they were nil), so the wire layout stays in lockstep across clients.
//
// Amsterdam: go-ethereum v1.17.6 adds BlockAccessListHash (EIP-7928) and
// SlotNumber (EIP-7843) after RequestsHash, in that order. Nethermind 2.1.0's
// HeaderDecoder reads the same two fields in the same order (requiredItems
// [6] and [7]), so the wrapper needs no Nethermind-specific type.
//
// Pinned bytes are caught by the differential oracle further down the
// pipeline, not here — this encoder's correctness is equivalent to the
// geth path's, which is already exercised in CI.
func EncodeHeader(h *types.Header) ([]byte, error) {
	return gethrlp.EncodeToBytes(h)
}
