package specbuild

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/spec"
)

// ResolveAddress picks an address for a spec entity via:
//  1. Explicit e.Address;
//  2. Name-derived: keccak256(seed||name)[12:];
//  3. Position-derived: keccak256(seed||"anon-N")[12:].
//
// Modes 1 and 2 are stable across YAML reorderings; mode 3 is not.
func ResolveAddress(seed int64, e spec.Entity, index int) common.Address {
	if e.Address != nil {
		return e.Address.Address()
	}
	var seedBytes [8]byte
	binary.BigEndian.PutUint64(seedBytes[:], uint64(seed))

	var key string
	if e.Name != "" {
		key = e.Name
	} else {
		key = fmt.Sprintf("anon-%d", index)
	}
	hash := crypto.Keccak256(seedBytes[:], []byte(key))
	return common.BytesToAddress(hash[12:])
}
