package autofill

import (
	"math"
	mrand "math/rand"
	"testing"
)

func TestSampleTruncatedNormal_InRange(t *testing.T) {
	rng := mrand.New(mrand.NewSource(42))
	sampler := Sampler{Mean: 5120, Stddev: 1700, Min: 1024, Max: 24576}
	for i := range 5000 {
		v := sampler.Draw(rng)
		if v < sampler.Min || v > sampler.Max {
			t.Fatalf("sample[%d]=%d out of range [%d, %d]", i, v, sampler.Min, sampler.Max)
		}
	}
}

func TestSampleTruncatedNormal_MeanWithin5Pct(t *testing.T) {
	rng := mrand.New(mrand.NewSource(42))
	sampler := Sampler{Mean: 5120, Stddev: 1700, Min: 1024, Max: 24576}
	const n = 10000
	var sum uint64
	for range n {
		sum += sampler.Draw(rng)
	}
	avg := float64(sum) / float64(n)
	if rel := math.Abs(avg-5120) / 5120; rel > 0.05 {
		t.Errorf("sample mean: got %.1f, want ~5120 within 5%% (got %.2f%% off)", avg, rel*100)
	}
}

func TestSampleTruncatedNormal_DegenerateRange(t *testing.T) {
	rng := mrand.New(mrand.NewSource(42))
	sampler := Sampler{Mean: 100, Stddev: 10, Min: 100, Max: 100}
	for range 100 {
		v := sampler.Draw(rng)
		if v != 100 {
			t.Fatalf("Min==Max sampler returned %d, want 100", v)
		}
	}
}

func TestSampleTruncatedNormal_HitsBoundsOnExtremeStddev(t *testing.T) {
	// Very wide stddev → most draws fall outside [min, max] → after 8 rejection
	// attempts the final fallback clamps to the bound. We can't predict which
	// bound (depends on the seed), but the sampler must not loop forever and
	// must return a value in range.
	rng := mrand.New(mrand.NewSource(7))
	sampler := Sampler{Mean: 50, Stddev: 1e6, Min: 10, Max: 100}
	for i := range 1000 {
		v := sampler.Draw(rng)
		if v < sampler.Min || v > sampler.Max {
			t.Fatalf("sample[%d]=%d out of range under extreme stddev", i, v)
		}
	}
}
