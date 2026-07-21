package flat

// Key encoders for the flat-state RocksDB.
//
// `path` arguments are the 32-byte packed nibble path (high nibble first, two
// nibbles per byte, zero-padded) exactly as produced by the trie Builder's
// packNibblesTo32 — byte-identical to Nethermind's TreePath.Path.Bytes.
// `pathLen` is the nibble count in [0, 64].
//
// Citations (src/Nethermind at tag 1.39.0 = 14aca2c520):
//   - Account/Storage keys: Nethermind.State.Flat/Persistence/BaseFlatPersistence.cs
//     (EncodeAccountKeyHashed, EncodeStorageKeyHashedWithShortPrefix)
//   - Node keys + CF routing: Nethermind.State.Flat/Persistence/BaseTriePersistence.cs
//     (EncodeStateTopNodeKey / EncodeShortenedStateNodeKey / EncodeFullStateNodeKey /
//      EncodeShortenedStorageNodeKey / EncodeFullStorageNodeKey; SetStateTrieNode /
//      SetStorageTrieNode dispatch), Nethermind.Trie/TreePath.cs (EncodeWith8Byte).

const (
	// Flat Account/Storage row key geometry.
	accountKeyLen         = 20 // keccak(addr)[0:20]
	storagePrefixPortion  = 4  // BasePersistence.StoragePrefixPortion
	storageSlotHashLen    = 32
	storagePostfixPortion = 16                                                                // remaining address-hash bytes stored at the tail
	storageKeyLen         = storagePrefixPortion + storageSlotHashLen + storagePostfixPortion // 52

	// Node-CF routing thresholds on nibble path length (BaseTriePersistence.cs).
	stateTopThreshold  = 5  // pathLen <= 5  → StateTopNodes
	shortenedThreshold = 15 // pathLen <= 15 → StateNodes / StorageNodes; else FallbackNodes

	// Node key lengths.
	addrHashPrefixLen  = 20 // bytes of keccak(addr) embedded in storage-node keys
	stateTopKeyLen     = 3
	shortenedKeyLen    = 8
	fullStateKeyLen    = 1 + 32 + 1                                                                          // 34
	shortStorageKeyLen = storagePrefixPortion + shortenedKeyLen + (addrHashPrefixLen - storagePrefixPortion) // 28
	fullStorageKeyLen  = 1 + storagePrefixPortion + 32 + 1 + (addrHashPrefixLen - storagePrefixPortion)      // 54
)

// AccountKey returns the flat Account CF key: the first 20 bytes of
// keccak256(address). Mirrors BaseFlatPersistence.EncodeAccountKeyHashed.
func AccountKey(addrHash [32]byte) []byte {
	out := make([]byte, accountKeyLen)
	copy(out, addrHash[:accountKeyLen])
	return out
}

// StorageKey returns the 52-byte flat Storage CF key:
//
//	addrHash[0:4] ‖ slotKeyHash[0:32] ‖ addrHash[4:20]
//
// slotKeyHash is keccak256(BE32(slotIndex)). The split places most of the
// address hash after the slot hash so RocksDB's comparator can skip it.
// Mirrors BaseFlatPersistence.EncodeStorageKeyHashedWithShortPrefix.
func StorageKey(addrHash [32]byte, slotKeyHash [32]byte) []byte {
	out := make([]byte, storageKeyLen)
	copy(out[0:storagePrefixPortion], addrHash[0:storagePrefixPortion])
	copy(out[storagePrefixPortion:storagePrefixPortion+storageSlotHashLen], slotKeyHash[:])
	copy(out[storagePrefixPortion+storageSlotHashLen:], addrHash[storagePrefixPortion:storagePrefixPortion+storagePostfixPortion])
	return out
}

// StateNodeKey routes a state-trie node to its column family by nibble path
// length and returns the CF and the encoded key. Mirrors the
// SetStateTrieNode dispatch. path must be 32 bytes.
func StateNodeKey(path []byte, pathLen int) (Column, []byte) {
	switch {
	case pathLen <= stateTopThreshold:
		// StateTopNodes, 3 bytes: path[0], path[1], (path[2]&0xf0 | len&0x0f).
		out := make([]byte, stateTopKeyLen)
		copy(out, path[:stateTopKeyLen])
		out[stateTopKeyLen-1] = (out[stateTopKeyLen-1] & 0xf0) | byte(pathLen&0x0f)
		return ColStateTopNodes, out
	case pathLen <= shortenedThreshold:
		// StateNodes, 8 bytes via EncodeWith8Byte.
		return ColStateNodes, encodeWith8Byte(path, pathLen)
	default:
		// FallbackNodes state, 34 bytes: 0x00 ‖ path[0:32] ‖ len.
		out := make([]byte, fullStateKeyLen)
		out[0] = 0x00
		copy(out[1:1+32], path[:32])
		out[1+32] = byte(pathLen)
		return ColFallbackNodes, out
	}
}

// StorageNodeKey routes a storage-trie node to its column family by nibble
// path length and returns the CF and the encoded key. addrHash is
// keccak256(contract address). Mirrors the SetStorageTrieNode dispatch.
// path must be 32 bytes.
func StorageNodeKey(addrHash [32]byte, path []byte, pathLen int) (Column, []byte) {
	switch {
	case pathLen <= shortenedThreshold:
		// StorageNodes, 28 bytes: addrHash[0:4] ‖ EncodeWith8Byte ‖ addrHash[4:20].
		out := make([]byte, shortStorageKeyLen)
		copy(out[0:storagePrefixPortion], addrHash[0:storagePrefixPortion])
		copy(out[storagePrefixPortion:storagePrefixPortion+shortenedKeyLen], encodeWith8Byte(path, pathLen))
		copy(out[storagePrefixPortion+shortenedKeyLen:], addrHash[storagePrefixPortion:addrHashPrefixLen])
		return ColStorageNodes, out
	default:
		// FallbackNodes storage, 54 bytes:
		//   0x01 ‖ addrHash[0:4] ‖ path[0:32] ‖ len ‖ addrHash[4:20].
		out := make([]byte, fullStorageKeyLen)
		out[0] = 0x01
		copy(out[1:1+storagePrefixPortion], addrHash[0:storagePrefixPortion])
		copy(out[1+storagePrefixPortion:1+storagePrefixPortion+32], path[:32])
		out[1+storagePrefixPortion+32] = byte(pathLen)
		copy(out[1+storagePrefixPortion+32+1:], addrHash[storagePrefixPortion:addrHashPrefixLen])
		return ColFallbackNodes, out
	}
}

// encodeWith8Byte copies the first 8 bytes of the packed path and packs the
// nibble path length into the low 4 bits of the last byte (its high 4 bits
// still hold path data). Mirrors TreePath.EncodeWith8Byte. Used by both
// StateNodes and the middle segment of StorageNodes keys. Requires len(path) >= 8.
func encodeWith8Byte(path []byte, pathLen int) []byte {
	out := make([]byte, shortenedKeyLen)
	copy(out, path[:shortenedKeyLen])
	out[shortenedKeyLen-1] = (out[shortenedKeyLen-1] & 0xf0) | byte(pathLen&0x0f)
	return out
}
