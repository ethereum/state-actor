package flat

import (
	"bytes"
	"testing"
)

func mkHash(fill func(i int) byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = fill(i)
	}
	return h
}

func TestAccountKey(t *testing.T) {
	ah := mkHash(func(i int) byte { return byte(i) }) // 0,1,...,31
	got := AccountKey(ah)
	if len(got) != 20 {
		t.Fatalf("len=%d want 20", len(got))
	}
	if !bytes.Equal(got, ah[:20]) {
		t.Fatalf("AccountKey=%x want %x", got, ah[:20])
	}
}

func TestStorageKey(t *testing.T) {
	ah := mkHash(func(i int) byte { return byte(i) })        // 00..1f
	sh := mkHash(func(i int) byte { return byte(0xa0 + i) }) // a0..bf
	got := StorageKey(ah, sh)

	var want []byte
	want = append(want, ah[0:4]...)  // 00 01 02 03
	want = append(want, sh[:]...)    // a0..bf
	want = append(want, ah[4:20]...) // 04..13
	if len(got) != 52 {
		t.Fatalf("len=%d want 52", len(got))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("StorageKey mismatch\n got: %x\nwant: %x", got, want)
	}
}

func TestStateNodeKey_Top(t *testing.T) {
	path := make([]byte, 32)
	path[0], path[1], path[2] = 0x12, 0x34, 0x50
	for _, tc := range []struct {
		pathLen int
		wantB2  byte
	}{
		{0, 0x50}, // (0x50 & 0xf0) | 0
		{3, 0x53},
		{5, 0x55},
	} {
		col, key := StateNodeKey(path, tc.pathLen)
		if col != ColStateTopNodes {
			t.Fatalf("len %d: col=%v want ColStateTopNodes", tc.pathLen, col)
		}
		want := []byte{0x12, 0x34, tc.wantB2}
		if !bytes.Equal(key, want) {
			t.Fatalf("len %d: key=%x want %x", tc.pathLen, key, want)
		}
	}
}

func TestStateNodeKey_Shortened(t *testing.T) {
	path := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}
	path = append(path, make([]byte, 24)...) // pad to 32
	for _, tc := range []struct {
		pathLen int
		wantB7  byte
	}{
		{6, 0xf6},  // (0xf0 & 0xf0) | 6
		{15, 0xff}, // (0xf0 & 0xf0) | 15
	} {
		col, key := StateNodeKey(path, tc.pathLen)
		if col != ColStateNodes {
			t.Fatalf("len %d: col=%v want ColStateNodes", tc.pathLen, col)
		}
		want := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, tc.wantB7}
		if !bytes.Equal(key, want) {
			t.Fatalf("len %d: key=%x want %x", tc.pathLen, key, want)
		}
	}
}

func TestStateNodeKey_Fallback(t *testing.T) {
	path := mkHash(func(i int) byte { return byte(i + 1) }) // 01..20
	for _, pathLen := range []int{16, 20, 64} {
		col, key := StateNodeKey(path[:], pathLen)
		if col != ColFallbackNodes {
			t.Fatalf("len %d: col=%v want ColFallbackNodes", pathLen, col)
		}
		if len(key) != 34 {
			t.Fatalf("len %d: keylen=%d want 34", pathLen, len(key))
		}
		if key[0] != 0x00 {
			t.Fatalf("len %d: prefix=%x want 00", pathLen, key[0])
		}
		if !bytes.Equal(key[1:33], path[:]) {
			t.Fatalf("len %d: path bytes mismatch", pathLen)
		}
		if key[33] != byte(pathLen) {
			t.Fatalf("len %d: length byte=%d", pathLen, key[33])
		}
	}
}

func TestStorageNodeKey_Shortened(t *testing.T) {
	ah := mkHash(func(i int) byte { return byte(i) }) // 00..1f
	path := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x80}
	path = append(path, make([]byte, 24)...)
	col, key := StorageNodeKey(ah, path, 15)
	if col != ColStorageNodes {
		t.Fatalf("col=%v want ColStorageNodes", col)
	}
	var want []byte
	want = append(want, ah[0:4]...)                                     // 00 01 02 03
	want = append(want, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x8f) // (0x80&0xf0)|15
	want = append(want, ah[4:20]...)                                    // 04..13
	if len(key) != 28 {
		t.Fatalf("keylen=%d want 28", len(key))
	}
	if !bytes.Equal(key, want) {
		t.Fatalf("StorageNodeKey mismatch\n got: %x\nwant: %x", key, want)
	}
}

func TestStorageNodeKey_Fallback(t *testing.T) {
	ah := mkHash(func(i int) byte { return byte(i) })          // 00..1f
	path := mkHash(func(i int) byte { return byte(0x40 + i) }) // 40..5f
	col, key := StorageNodeKey(ah, path[:], 20)
	if col != ColFallbackNodes {
		t.Fatalf("col=%v want ColFallbackNodes", col)
	}
	if len(key) != 54 {
		t.Fatalf("keylen=%d want 54", len(key))
	}
	if key[0] != 0x01 {
		t.Fatalf("prefix=%x want 01", key[0])
	}
	if !bytes.Equal(key[1:5], ah[0:4]) {
		t.Fatalf("addr prefix mismatch")
	}
	if !bytes.Equal(key[5:37], path[:]) {
		t.Fatalf("path mismatch")
	}
	if key[37] != 20 {
		t.Fatalf("length byte=%d want 20", key[37])
	}
	if !bytes.Equal(key[38:54], ah[4:20]) {
		t.Fatalf("addr postfix mismatch")
	}
}

// TestNodeKeyRouting exercises every nibble length 0..64 and asserts the
// column-family selection and key length match the verified dispatch.
func TestNodeKeyRouting(t *testing.T) {
	path := make([]byte, 32)
	var ah [32]byte
	for l := 0; l <= 64; l++ {
		sc, sk := StateNodeKey(path, l)
		switch {
		case l <= 5:
			if sc != ColStateTopNodes || len(sk) != 3 {
				t.Fatalf("state len %d: col=%v klen=%d", l, sc, len(sk))
			}
		case l <= 15:
			if sc != ColStateNodes || len(sk) != 8 {
				t.Fatalf("state len %d: col=%v klen=%d", l, sc, len(sk))
			}
		default:
			if sc != ColFallbackNodes || len(sk) != 34 {
				t.Fatalf("state len %d: col=%v klen=%d", l, sc, len(sk))
			}
		}
		tc, tk := StorageNodeKey(ah, path, l)
		switch {
		case l <= 15:
			if tc != ColStorageNodes || len(tk) != 28 {
				t.Fatalf("storage len %d: col=%v klen=%d", l, tc, len(tk))
			}
		default:
			if tc != ColFallbackNodes || len(tk) != 54 {
				t.Fatalf("storage len %d: col=%v klen=%d", l, tc, len(tk))
			}
		}
	}
}
