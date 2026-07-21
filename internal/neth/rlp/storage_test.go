package rlp

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestEncodeStorageValue pins the trimmed-then-RLP encoding of storage slot
// values. Every expected output is derived from the standard RLP spec, not
// from the implementation.
func TestEncodeStorageValue(t *testing.T) {
	cases := []struct {
		name    string
		in      string // hex hash literal
		want    string // hex RLP output
		wantNil bool
	}{
		{name: "zero", in: "0x0", wantNil: true},
		{name: "one", in: "0x1", want: "01"},              // single byte < 0x80
		{name: "0x7f", in: "0x7f", want: "7f"},            // single byte < 0x80
		{name: "0x80", in: "0x80", want: "8180"},          // single byte >= 0x80
		{name: "two-bytes", in: "0x0102", want: "820102"}, // 2-byte string
		{name: "full-32-ff", in: "0x" + hexRepeat("ff", 32), // 33-byte RLP (0xa0 prefix)
			want: "a0" + hexRepeat("ff", 32)},
	}
	for _, c := range cases {
		got, err := EncodeStorageValue(common.HexToHash(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.wantNil {
			if got != nil {
				t.Errorf("%s: got %x want nil", c.name, got)
			}
			continue
		}
		want, err := hex.DecodeString(c.want)
		if err != nil {
			t.Fatalf("%s: bad fixture: %v", c.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %x want %s", c.name, got, c.want)
		}
	}

	// The all-0xff value is the SlotEncoding pitfall probe: it RLP-encodes to
	// exactly 33 bytes, which a raw-mode reader rejects. Confirm the shape.
	full, err := EncodeStorageValue(common.HexToHash("0x" + hexRepeat("ff", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 33 || full[0] != 0xa0 {
		t.Fatalf("full-ff = %x (len %d), want 33-byte 0xa0-prefixed string", full, len(full))
	}
}

func hexRepeat(b string, n int) string {
	out := make([]byte, 0, len(b)*n)
	for range n {
		out = append(out, b...)
	}
	return string(out)
}
