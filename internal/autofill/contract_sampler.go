package autofill

import (
	mrand "math/rand"
)

// truncatedNormalMaxAttempts caps the rejection-sampling loop. After this
// many out-of-range draws, sampleTruncatedNormal clamps the next draw to
// the bound — bounded attempt count keeps a single seeded run from looping
// arbitrarily long on a pathological (mean, stddev, range) combination.
const truncatedNormalMaxAttempts = 8

// Sampler draws a uint64 from N(Mean, Stddev) clamped to [Min, Max].
type Sampler struct {
	Mean   float64
	Stddev float64
	Min    uint64
	Max    uint64
}

// Draw returns a sample using the configured truncated-normal distribution.
// The number of RNG draws is bounded by truncatedNormalMaxAttempts+1.
func (s Sampler) Draw(rng *mrand.Rand) uint64 {
	return sampleTruncatedNormal(rng, s.Mean, s.Stddev, s.Min, s.Max)
}

func sampleTruncatedNormal(rng *mrand.Rand, mean, stddev float64, min, max uint64) uint64 {
	if min >= max {
		return min
	}
	fmin, fmax := float64(min), float64(max)
	var v float64
	for range truncatedNormalMaxAttempts {
		v = mean + rng.NormFloat64()*stddev
		if v >= fmin && v <= fmax {
			return uint64(v)
		}
	}
	if v < fmin {
		return min
	}
	if v > fmax {
		return max
	}
	return uint64(v)
}
