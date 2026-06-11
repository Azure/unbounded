// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"fmt"
	"math/bits"
	"math/rand"

	flag "github.com/spf13/pflag"
)

// zipfConfig holds the tunables that govern which object indices are read and
// how frequently. The same configuration on every soaks3 instance yields the
// same hot-key set, so cache behavior is reproducible across the cluster.
type zipfConfig struct {
	// s is the Zipf skew exponent. Must be > 1. Larger values concentrate
	// load on fewer keys.
	s float64
	// v is the Zipf v parameter. Must be >= 1.
	v float64
	// seed is the base seed for per-worker random streams.
	seed int64
	// uniform selects a uniform distribution instead of Zipf when true.
	uniform bool
	// shuffleSeed keys the index permutation. A shared default keeps the hot
	// key set identical cluster-wide; the permutation scatters those hot keys
	// across the key space so they are not all clustered at low indices.
	shuffleSeed int64
}

// defaultZipfConfig returns the documented default tunables.
func defaultZipfConfig() zipfConfig {
	return zipfConfig{
		s:           1.1,
		v:           1.0,
		seed:        1,
		uniform:     false,
		shuffleSeed: 0x5061726B, // shared constant so hot keys match across instances
	}
}

// registerZipfFlags wires the distribution tunables onto a flag set.
func registerZipfFlags(fs *flag.FlagSet, cfg *zipfConfig) {
	fs.Float64Var(&cfg.s, "zipf-s", cfg.s, "Zipf skew exponent (must be > 1)")
	fs.Float64Var(&cfg.v, "zipf-v", cfg.v, "Zipf v parameter (must be >= 1)")
	fs.Int64Var(&cfg.seed, "seed", cfg.seed, "Base seed for per-worker random streams")
	fs.BoolVar(&cfg.uniform, "uniform", cfg.uniform, "Use a uniform distribution instead of Zipf")
	fs.Int64Var(&cfg.shuffleSeed, "shuffle-seed", cfg.shuffleSeed, "Seed for the rank-to-key index permutation (shared across the cluster)")
}

// validate checks the distribution parameters.
func (c zipfConfig) validate() error {
	if !c.uniform {
		if c.s <= 1 {
			return fmt.Errorf("--zipf-s must be > 1, got %g", c.s)
		}

		if c.v < 1 {
			return fmt.Errorf("--zipf-v must be >= 1, got %g", c.v)
		}
	}

	return nil
}

// selector draws object indices in [0, count) according to the configured
// distribution. It is not safe for concurrent use; build one per worker.
type selector struct {
	count int64
	rng   *rand.Rand
	zipf  *rand.Zipf
	perm  *permutation
}

// newSelector builds a per-worker selector. worker is mixed into the seed so
// each worker draws an independent stream while remaining deterministic.
func (c zipfConfig) newSelector(count int64, worker int) (*selector, error) {
	if count <= 0 {
		return nil, fmt.Errorf("count must be positive, got %d", count)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	rng := rand.New(rand.NewSource(c.seed + int64(worker)))

	s := &selector{count: count, rng: rng}
	if !c.uniform {
		// imax is inclusive, so the largest index drawn is count-1.
		s.zipf = rand.NewZipf(rng, c.s, c.v, uint64(count-1))
		if s.zipf == nil {
			return nil, fmt.Errorf("invalid zipf parameters s=%g v=%g", c.s, c.v)
		}
	}

	if c.shuffleSeed != 0 {
		s.perm = newPermutation(uint64(count), uint64(c.shuffleSeed))
	}

	return s, nil
}

// pick returns the next object index.
func (s *selector) pick() int64 {
	var rank uint64
	if s.zipf != nil {
		rank = s.zipf.Uint64()
	} else {
		rank = uint64(s.rng.Int63n(s.count))
	}

	if s.perm != nil {
		rank = s.perm.permute(rank)
	}

	return int64(rank)
}

// permutation is a bijection on [0, n) built from a balanced Feistel network
// with cycle-walking. It scatters ranks across the key space without storing
// an O(n) table, so it works for very large data sets with bounded memory.
type permutation struct {
	n        uint64
	halfBits uint
	mask     uint64
	keys     [feistelRounds]uint64
}

const feistelRounds = 4

// newPermutation constructs a permutation over [0, n) keyed by seed.
func newPermutation(n, seed uint64) *permutation {
	if n <= 1 {
		return &permutation{n: n, halfBits: 0, mask: 0}
	}

	// totalBits is the smallest even bit count whose domain covers n.
	totalBits := uint(bits.Len64(n - 1))
	if totalBits%2 != 0 {
		totalBits++
	}

	half := totalBits / 2

	p := &permutation{
		n:        n,
		halfBits: half,
		mask:     (uint64(1) << half) - 1,
	}

	for i := range p.keys {
		// Derive each round key deterministically from the seed so the
		// permutation is identical across processes and runs.
		p.keys[i] = mix(seed + uint64(i+1)*0x9E3779B97F4A7C15)
	}

	return p
}

// permute maps x in [0, n) to a unique value in [0, n).
func (p *permutation) permute(x uint64) uint64 {
	if p.n <= 1 {
		return 0
	}

	x %= p.n

	// Cycle-walk: apply the Feistel round function until the output lands
	// inside [0, n). Because the function is a bijection on the power-of-two
	// domain, this terminates and stays a bijection on [0, n).
	for {
		x = p.feistel(x)
		if x < p.n {
			return x
		}
	}
}

// feistel applies the balanced Feistel rounds over the power-of-two domain.
func (p *permutation) feistel(x uint64) uint64 {
	left := (x >> p.halfBits) & p.mask
	right := x & p.mask

	for i := 0; i < feistelRounds; i++ {
		f := mix(right^p.keys[i]) & p.mask
		left, right = right, left^f
	}

	return (left << p.halfBits) | right
}

// mix is a 64-bit avalanche function (splitmix64 finalizer).
func mix(z uint64) uint64 {
	z += 0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB

	return z ^ (z >> 31)
}
