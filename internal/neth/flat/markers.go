package flat

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Metadata CF marker keys are keccak256 of the ASCII marker name. Mirrors
// Nethermind.State.Flat/Persistence/BasePersistence.cs @ tag 1.39.0:
//
//	CurrentStateKey = Keccak.Compute("CurrentState")
//	LayoutKey       = Keccak.Compute("Layout")
//	SlotEncodingKey = Keccak.Compute("SlotEncoding")
//
// The verified hex values are pinned byte-for-byte in markers_test.go, which
// also recomputes them from the ASCII names to guard against a wrong string.
var (
	CurrentStateKey = crypto.Keccak256([]byte("CurrentState"))
	LayoutKey       = crypto.Keccak256([]byte("Layout"))
	SlotEncodingKey = crypto.Keccak256([]byte("SlotEncoding"))
)

const (
	// LayoutFlat is the FlatLayout.Flat enum value — the default (and only
	// state-actor-emitted) flat layout. A stored Layout that differs from the
	// value Nethermind is configured with makes it refuse to start.
	LayoutFlat byte = 0x00

	// SlotEncodingRLP marks storage slot values as RLP-wrapped (the 1.39.0+
	// format). It MUST be written whenever the Storage CF holds RLP-wrapped
	// values: absent this marker with a non-empty Storage CF, Nethermind reads
	// the slots as legacy raw bytes and throws on any 33-byte RLP value.
	SlotEncodingRLP byte = 0x01
)

// CurrentStateValue encodes the 40-byte CurrentState marker value:
//
//	int64 big-endian block number ‖ 32-byte state root
//
// A value with a block number >= 0 is exactly what makes Nethermind's
// FlatStateActivationPolicy report "State backend: flat (existing flat DB
// detected)". state-actor writes (0, genesisStateRoot). Mirrors
// BasePersistence.SetCurrentState.
func CurrentStateValue(blockNumber int64, root common.Hash) []byte {
	out := make([]byte, 8+common.HashLength)
	binary.BigEndian.PutUint64(out[:8], uint64(blockNumber))
	copy(out[8:], root[:])
	return out
}
