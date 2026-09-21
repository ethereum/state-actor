package autofill

import (
	"bytes"
	"compress/flate"
	mrand "math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/internal/sizecal"
)

func deflated(b []byte) float64 {
	var buf bytes.Buffer
	// BestSpeed matches Deflater.BEST_SPEED, the level LiveGeom.java/DumpCode.java used to
	// measure the mainnet targets below — a different level gives a different ratio for the
	// same bytes, so this has to match the tool that produced the target, not the strongest
	// available compression.
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(b)
	w.Close()
	return float64(buf.Len()) / float64(len(b))
}

// packedRandom mirrors how a RocksDB data block actually fills: reps of randomly-selected pool
// entries appended until the block is full, not a run of consecutive pool indices (adjacent
// pool indices are never adjacent on disk — the store is keyed by code hash).
func packedRandom(pool [][]byte, limit int) float64 {
	rnd := mrand.New(mrand.NewSource(7))
	var raw, comp int
	for range 500 {
		var buf bytes.Buffer
		for buf.Len() < limit {
			buf.Write(pool[rnd.Intn(len(pool))])
		}
		raw += buf.Len()
		comp += int(deflated(buf.Bytes()) * float64(buf.Len()))
	}
	return float64(comp) / float64(raw)
}

// TestPoolCode_CompressesLikeMainnet pins the code pool's compressibility against a live
// remeasurement of mainnet's own CODE_STORAGE column family (corpus_meta.json,
// "mainnet_gates_measured_2026-09-21"). The prior implementation tiled one contract, compressed
// to ~0.06-0.2, and produced a store that read code 46-51% faster than mainnet; these gates
// catch a regression back toward that, not merely "shared code exists".
//
// per-record is the primary gate (corroborated against mainnet directly). packed-random-32KiB
// is reported (t.Logf) rather than gated pass/fail: mainnet's own packed-block figure varied
// 0.415-0.439 across two same-day runs on the same store, i.e. its measurement noise is wider
// than a tight accept band would tolerate; a gross regression (tiling) still fails it via the
// wide sanity band below.
func TestPoolCode_CompressesLikeMainnet(t *testing.T) {
	s := Sampler{Mean: float64(sizecal.MeanContractCode), Stddev: float64(sizecal.MeanContractCode) / 3,
		Min: sizecal.MinContractCode, Max: sizecal.MaxContractCode}
	const n = 2000
	pool := make([][]byte, n)
	seen := map[common.Hash]struct{}{}
	var raw, comp int
	for j := range n {
		code, hash := poolCode(j, s)
		pool[j] = code
		seen[hash] = struct{}{}
		if l := len(code); l < int(s.Min) || l > int(s.Max) {
			t.Fatalf("entry %d: %d B outside [%d, %d]", j, l, s.Min, s.Max)
		}
		raw += len(code)
		comp += int(deflated(code) * float64(len(code)))
	}

	if len(seen) != n {
		t.Fatalf("distinct hashes %d, want %d (every draw must hash uniquely)", len(seen), n)
	}

	perRecord := float64(comp) / float64(raw)
	t.Logf("per-record deflate %.4f (mainnet 0.443)", perRecord)
	if perRecord < 0.443*0.95 || perRecord > 0.443*1.05 {
		t.Fatalf("per-record deflate %.4f, want within ±5%% of mainnet's 0.443", perRecord)
	}

	packed := packedRandom(pool, 32<<10)
	t.Logf("packed-random-32KiB deflate %.4f (mainnet measured 0.415-0.439)", packed)
	if packed < 0.25 {
		t.Fatalf("packed-random-32KiB deflate %.4f: entries are compressing against each other (tiling regression)", packed)
	}
}
