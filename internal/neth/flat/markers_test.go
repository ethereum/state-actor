package flat

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestMarkerKeys pins the three Metadata-CF marker keys against the verified
// keccak256(ASCII) values (independently recomputed with `cast keccak`). Since
// the package derives them via crypto.Keccak256([]byte("<name>")), a mismatch
// here means either a wrong ASCII marker name or a keccak regression.
func TestMarkerKeys(t *testing.T) {
	cases := []struct {
		name string
		key  []byte
		want string
	}{
		{"CurrentState", CurrentStateKey, "6d9c1cfe12ab0d61b481bf665c5b07838268a72ee3eaa7fbb9a425a73f07600d"},
		{"Layout", LayoutKey, "e2f739fb107f4e27824287dfd2a5f5c7172a03c8cb1f867834564936b1a24a12"},
		{"SlotEncoding", SlotEncodingKey, "09bb0de64c1d900d7ca4ea69e789fa38a9ce66a034cf43b99dd6e830c25566b7"},
	}
	for _, c := range cases {
		want, err := hex.DecodeString(c.want)
		if err != nil {
			t.Fatalf("%s: bad fixture: %v", c.name, err)
		}
		if len(c.key) != 32 {
			t.Errorf("%s: key len=%d want 32", c.name, len(c.key))
		}
		if !bytes.Equal(c.key, want) {
			t.Errorf("%s key = %x want %s", c.name, c.key, c.want)
		}
	}
}

func TestMarkerConsts(t *testing.T) {
	if LayoutFlat != 0x00 {
		t.Errorf("LayoutFlat=%#x want 0x00", LayoutFlat)
	}
	if SlotEncodingRLP != 0x01 {
		t.Errorf("SlotEncodingRLP=%#x want 0x01", SlotEncodingRLP)
	}
}

func TestCurrentStateValue(t *testing.T) {
	root := common.HexToHash("0x00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")

	v := CurrentStateValue(0, root)
	if len(v) != 40 {
		t.Fatalf("len=%d want 40", len(v))
	}
	if !bytes.Equal(v[:8], []byte{0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("block 0 header = %x want 8 zero bytes", v[:8])
	}
	if !bytes.Equal(v[8:], root[:]) {
		t.Errorf("root segment = %x want %x", v[8:], root[:])
	}

	// Big-endian block-number encoding.
	if got := CurrentStateValue(1, root)[:8]; !bytes.Equal(got, []byte{0, 0, 0, 0, 0, 0, 0, 1}) {
		t.Errorf("block 1 header = %x", got)
	}
	if got := CurrentStateValue(256, root)[:8]; !bytes.Equal(got, []byte{0, 0, 0, 0, 0, 0, 1, 0}) {
		t.Errorf("block 256 header = %x", got)
	}
}
