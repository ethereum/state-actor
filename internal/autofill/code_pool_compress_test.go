package autofill

import (
	"bytes"
	"compress/flate"
	"sort"
	"testing"

	"github.com/ethereum/state-actor/internal/sizecal"
)

func deflated(b []byte) float64 {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(b)
	w.Close()
	return float64(buf.Len()) / float64(len(b))
}

// The pool's job is to compress like mainnet code, not merely to be shared. Mainnet code
// records deflate to about 0.44; one runtime tiled to size deflated to about 0.2 and produced
// a store that read code 46% faster than mainnet, while random bytes (the defect before that)
// do not compress at all. This pins the property at the two scales that matter: entries on
// their own, and a data block's worth of entries packed in code-hash order, the way the store
// packs them. Single entries are not bounded: a real contract can legitimately deflate below
// 0.25 on its own.
func TestPoolCode_CompressesLikeMainnet(t *testing.T) {
	s := Sampler{Mean: float64(sizecal.MeanContractCode), Stddev: float64(sizecal.MeanContractCode) / 3,
		Min: sizecal.MinContractCode, Max: sizecal.MaxContractCode}
	const n = 400
	var raw, comp int
	codes := make([][]byte, n)
	hashes := make([][]byte, n)
	for j := range n {
		code, h := poolCode(j, s)
		raw += len(code)
		comp += int(deflated(code) * float64(len(code)))
		codes[j], hashes[j] = code, h[:]
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return bytes.Compare(hashes[order[a]], hashes[order[b]]) < 0 })
	var block bytes.Buffer
	for _, i := range order {
		if block.Len() >= 32<<10 {
			break
		}
		block.Write(codes[i])
	}
	t.Logf("per-entry deflate %.4f, hash-ordered block %.4f", float64(comp)/float64(raw), deflated(block.Bytes()))
	if r := float64(comp) / float64(raw); r < 0.30 || r > 0.60 {
		t.Fatalf("per-entry deflate %.3f, want within 0.30-0.60 (mainnet 0.443)", r)
	}
	if r := deflated(block.Bytes()); r < 0.25 {
		t.Fatalf("hash-ordered block deflates to %.3f: entries share a source", r)
	}
}
