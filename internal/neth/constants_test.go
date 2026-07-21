package neth

import (
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// TestKeccakConstants_DerivedFromSpec verifies the three pinned hashes by
// recomputing them from their canonical input bytes. If any hash drifts
// from this expected value, the constants in constants.go are wrong (or
// the underlying keccak primitive was swapped).
func TestKeccakConstants_DerivedFromSpec(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want [32]byte
	}{
		{"OfAnEmptyString = keccak([])", []byte{}, OfAnEmptyString},
		{"EmptyTreeHash = keccak([0x80])", []byte{0x80}, EmptyTreeHash},
		{"OfAnEmptySequenceRlp = keccak([0xc0])", []byte{0xc0}, OfAnEmptySequenceRlp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := crypto.Keccak256Hash(c.in)
			if got != c.want {
				t.Fatalf("constant mismatch:\n  got:  %s\n  want: %x", got.Hex(), c.want[:])
			}
		})
	}
}
