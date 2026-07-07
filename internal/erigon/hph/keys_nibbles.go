// Vendored from github.com/erigontech/erigon execution/commitment/keys_nibbles.go @ 14273f79a6 (production pin).
// Modifications: package commitment -> hph; build tag; nibbles import rewrite; R2 strip: KeyToNibblizedHash/CompactKey/PrefixStringToNibbles/NibblesToString
//
//go:build cgo_erigon_commitment

package hph

import (
	"fmt"
	"strings"

	keccak "github.com/erigontech/fastkeccak"

	"github.com/erigontech/erigon/common/length"
)

// KeyToHexNibbleHash hashes plain key with respect to plain key size (part < 20 bytes for account, part >= 20 bytes for storage)
// and returns the hashed key in nibblized form suitable for hex trie (each byte represented by 2 nibbles).
func KeyToHexNibbleHash(key []byte) []byte {
	// `nibblized`, `hashed` - are the same array
	// but `hashed` is 2nd half of `nibblized`
	// will use 1st half of `nibblized` in the end
	var nibblized, hashed []byte
	if len(key) > length.Addr { // storage
		nibblized = make([]byte, 128)
		hashed = nibblized[64:]
		h := keccak.Sum256(key[:length.Addr])
		copy(hashed[:32], h[:])
		h = keccak.Sum256(key[length.Addr:])
		copy(hashed[32:], h[:])
	} else {
		nibblized = make([]byte, 64)
		hashed = nibblized[32:]
		h := keccak.Sum256(key)
		copy(hashed, h[:])
	}

	for i, b := range hashed {
		nibblized[i*2] = (b >> 4) & 0xf
		nibblized[i*2+1] = b & 0xf
	}
	return nibblized
}

// updatedNibs returns a string of nibbles that are set in the given number.
func updatedNibs(num uint16) string {
	var nibbles []string
	for i := 0; i < 16; i++ {
		if num&(1<<i) != 0 {
			nibbles = append(nibbles, fmt.Sprintf("%X", i))
		}
	}
	return strings.Join(nibbles, ",")
}

// hashes plainKey using keccakState and writes the hashed key nibbles to dest with respect to hashedKeyOffset.
// Note that this function does not respect plainKey length so hashing it at once without splitting to account/storage part.
func hashKey(hasher keccak.KeccakState, plainKey []byte, dest []byte, hashedKeyOffset int16, hashBuf []byte) error {
	_, _ = hashBuf[length.Hash-1], dest[length.Hash*2-1] // bounds checks elimination
	hasher.Reset()
	if _, err := hasher.Write(plainKey); err != nil {
		return err
	}
	if _, err := hasher.Read(hashBuf); err != nil {
		return err
	}
	hb := hashBuf[hashedKeyOffset/2:]
	var k int
	if hashedKeyOffset%2 == 1 { // write zero byte as compacted since hashedKeyOffset is odd
		dest[0] = hb[0] & 0xf
		k++
		hb = hb[1:]
	}
	// write each byte as 2 hex nibbles
	for _, c := range hb {
		dest[k] = (c >> 4) & 0xf
		k++
		dest[k] = c & 0xf
		k++
	}
	return nil
}
