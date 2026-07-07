package recsplit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fixtureEntry mirrors the JSON schema written by
// upstream Erigon's reference recsplit encoder.
type fixtureEntry struct {
	KeyHex string `json:"key_hex"`
	Offset uint64 `json:"offset"`
}

type fixture struct {
	Label       string         `json:"label"`
	KeyCount    int            `json:"key_count"`
	BucketSize  int            `json:"bucket_size"`
	LeafSize    uint16         `json:"leaf_size"`
	Salt        uint32         `json:"salt"`
	BaseDataID  uint64         `json:"base_data_id"`
	Enums       bool           `json:"enums"`
	Entries     []fixtureEntry `json:"entries"`
	ExpectedHex string         `json:"expected_hex"`
}

// TestRecSplit_Spike is the load-bearing byte-equality check for the
// spike: produce a .kvi from a fixture and assert byte-identical output
// against Erigon's reference writer.
//
// The golden fixtures are committed under testdata/; they were captured
// from upstream Erigon v3.4.2's reference RecSplit writer.
//
// This test is the GATE for the spike: if it fails, the recommended
// fallback is plan Task 33 (vendor github.com/erigontech/erigon/db/recsplit).
//
// spike_100.json is the GATE fixture (single bucket — exercises
// findBijection within a leaf-sized partition path). spike_1000.json
// extends coverage to the recursive-split path (bucketSize=100 → ~10
// buckets, each ~100 keys → at least one level of splitParams recursion).
func TestRecSplit_Spike(t *testing.T) {
	// The 100 / 1000 pair exercises the structurally-distinct encoder
	// paths: leaf bijection (100) and recursive split (1000). The larger
	// spike_10000.json (1.2 MB) was dropped from the checked-in trio — its
	// at-scale coverage is provided end-to-end by TestE2ESuite, which boots
	// the real erigon daemon to read the .kvi. spike_100.json is also
	// consumed by hash_test.go (TestKeyHashAgainstFixture).
	// workers=0 pins the sequential path, workers=4 the parallel one —
	// same .kvi golden for both (the in-order consumer reproduces the
	// sequential concatenation; spike_1000's 10 buckets exercise the
	// heap-merge ordering).
	for _, name := range []string{"spike_100.json", "spike_1000.json"} {
		for _, workers := range []int{0, 4} {
			t.Run(fmt.Sprintf("%s/workers=%d", name, workers), func(t *testing.T) {
				runSpikeFixture(t, filepath.Join("testdata", name), workers)
			})
		}
	}
}

func runSpikeFixture(t *testing.T, path string, workers int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (golden is committed under testdata/)", path, err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	want, err := hex.DecodeString(f.ExpectedHex)
	if err != nil {
		t.Fatalf("decode expected hex: %v", err)
	}

	tmp := t.TempDir()
	idxPath := filepath.Join(tmp, "spike.kvi")
	salt := f.Salt

	w, err := New(Args{
		KeyCount:   f.KeyCount,
		BucketSize: f.BucketSize,
		LeafSize:   f.LeafSize,
		Salt:       &salt,
		TmpDir:     tmp,
		IndexFile:  idxPath,
		BaseDataID: f.BaseDataID,
		Enums:      f.Enums,
		Workers:    workers,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	for _, e := range f.Entries {
		k, err := hex.DecodeString(e.KeyHex)
		if err != nil {
			t.Fatalf("decode key hex %q: %v", e.KeyHex, err)
		}
		if err := w.AddKey(k, e.Offset); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
	}

	if err := w.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v (collision=%v)", err, w.Collision())
	}

	got, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read produced .kvi: %v", err)
	}

	if bytes.Equal(got, want) {
		t.Logf("SPIKE GREEN (%s): produced %d bytes byte-identical to Erigon's output",
			filepath.Base(path), len(got))
		return
	}

	// Diff diagnostics for SPIKE_RED triage.
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	firstDiff := -1
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			firstDiff = i
			break
		}
	}

	// Decompose to identify which section diverges.
	section := classifyOffset(firstDiff, want)

	t.Fatalf("SPIKE RED (%s): byte mismatch (section=%s)\n"+
		"  first divergence at byte %d\n"+
		"  got  len=%d head=%x ... around-diff=%x\n"+
		"  want len=%d head=%x ... around-diff=%x",
		filepath.Base(path), section, firstDiff,
		len(got), headSafe(got, 24), windowSafe(got, firstDiff, 16),
		len(want), headSafe(want, 24), windowSafe(want, firstDiff, 16),
	)
}

// classifyOffset returns a string indicating which logical section of
// the .kvi file the byte offset falls in. Helps a failing spike pinpoint
// whether the bug is in the hash layer (offsetData), the recsplit
// recursion (golomb-rice section), or the bucket accumulators (ef section).
func classifyOffset(off int, b []byte) string {
	if off < 0 {
		return "unknown"
	}
	if off < 17 {
		return "header"
	}
	if len(b) < 17 {
		return "header"
	}
	keyCount := int(uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15]))
	bpr := int(b[16])
	dataEnd := 17 + keyCount*bpr
	if off < dataEnd {
		return "offsetData (bucket-shuffled record bytes)"
	}
	// Footer: bucketCount(8)+bucketSize(2)+leafSize(2)+salt(4)+1+seedsN*8 + features(1)
	// + 4B grSize + 8B grLen + grLen*8B grData + 40B ef header + ef data
	post := dataEnd
	if off < post+8 {
		return "footer: bucketCount"
	}
	if off < post+10 {
		return "footer: bucketSize"
	}
	if off < post+12 {
		return "footer: leafSize"
	}
	if off < post+16 {
		return "footer: salt"
	}
	post += 16
	if len(b) <= post {
		return "footer-truncated"
	}
	ssn := int(b[post])
	post++
	if off < post+ssn*8 {
		return "footer: startSeeds"
	}
	post += ssn * 8
	if off < post+1 {
		return "footer: features"
	}
	post++
	if off < post+4 {
		return "footer: grParamCount (uint16 in 4B slot)"
	}
	post += 4
	if off < post+8 {
		return "gr: data-length header"
	}
	if len(b) < post+8 {
		return "gr-truncated"
	}
	grLen := int(uint64(b[post])<<56 | uint64(b[post+1])<<48 | uint64(b[post+2])<<40 | uint64(b[post+3])<<32 |
		uint64(b[post+4])<<24 | uint64(b[post+5])<<16 | uint64(b[post+6])<<8 | uint64(b[post+7]))
	post += 8
	if off < post+grLen*8 {
		return "gr: data (golomb-rice bit-stream, LE uint64)"
	}
	post += grLen * 8
	if off < post+8 {
		return "ef: numBuckets"
	}
	if off < post+16 {
		return "ef: uCumKeys"
	}
	if off < post+24 {
		return "ef: uPosition"
	}
	if off < post+32 {
		return "ef: cumKeysMinDelta"
	}
	if off < post+40 {
		return "ef: posMinDelta"
	}
	return "ef: data (DoubleEliasFano payload, LE uint64)"
}

func headSafe(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func windowSafe(b []byte, center, half int) []byte {
	if center < 0 {
		return nil
	}
	lo := center - half
	if lo < 0 {
		lo = 0
	}
	hi := center + half
	if hi > len(b) {
		hi = len(b)
	}
	return b[lo:hi]
}
