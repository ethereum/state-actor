package nimbus

import (
	"encoding/binary"
	"fmt"
)

// SavedState is Aristo's admin record, stored under the EMPTY key in AriVtx
// (aristo_init/rocks_db/rdb_desc.nim AdmKey): the dynamic-ID high-water mark
// and the block number the persisted state belongs to.
type SavedState struct {
	VTop   uint64 // last dynamic vertex ID in use, 0 if none
	Serial uint64 // block number of the persisted state (0 at genesis)
}

// AdminKey is the AriVtx key of the SavedState record.
var AdminKey = []byte{}

// EncodeSavedState returns BE64(vTop) ‖ BE64(serial) ‖ SavedStateMagic.
func EncodeSavedState(s SavedState) []byte {
	out := make([]byte, SavedStateLen)
	binary.BigEndian.PutUint64(out[0:8], s.VTop)
	binary.BigEndian.PutUint64(out[8:16], s.Serial)
	out[16] = SavedStateMagic
	return out
}

// DecodeSavedState reverses EncodeSavedState (accepting the legacy trailer).
func DecodeSavedState(b []byte) (SavedState, error) {
	if len(b) != SavedStateLen {
		return SavedState{}, fmt.Errorf("nimbus: SavedState must be %d bytes (got %d)", SavedStateLen, len(b))
	}
	if b[16] != SavedStateMagic && b[16] != SavedStateLegacyMagic {
		return SavedState{}, fmt.Errorf("nimbus: SavedState trailer 0x%02x is not 0x%02x/0x%02x", b[16], SavedStateMagic, SavedStateLegacyMagic)
	}
	return SavedState{
		VTop:   binary.BigEndian.Uint64(b[0:8]),
		Serial: binary.BigEndian.Uint64(b[8:16]),
	}, nil
}
