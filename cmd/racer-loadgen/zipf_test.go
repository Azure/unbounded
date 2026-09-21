// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func TestZipfDistributionAndDeterminism(t *testing.T) {
	// Independently tabulated probabilities for four ranked objects.
	for _, tc := range []struct {
		exponent      float64
		probabilities [4]float64
	}{
		{0, [4]float64{.25, .25, .25, .25}},
		{.5, [4]float64{.3591364427, .2539478140, .2073475219, .1795682214}},
		{1, [4]float64{.48, .24, .16, .12}},
		{2, [4]float64{144.0 / 205, 36.0 / 205, 16.0 / 205, 9.0 / 205}},
	} {
		t.Run(fmt.Sprint(tc.exponent), func(t *testing.T) {
			z := newZipf(4, tc.exponent)
			r1, r2 := rand.New(rand.NewSource(123)), rand.New(rand.NewSource(123))
			r3 := rand.New(rand.NewSource(456))

			var counts [4]int

			different := false

			const draws = 100_000
			for i := 0; i < draws; i++ {
				got := z.sample(r1)
				if got < 0 || got >= len(counts) {
					t.Fatalf("sample out of range: %d", got)
				}

				if other := z.sample(r2); got != other {
					t.Fatalf("same seed diverged at draw %d: %d != %d", i, got, other)
				}

				different = z.sample(r3) != got || different
				counts[got]++
			}

			if !different {
				t.Error("different seeds produced identical sequences")
			}

			for rank, want := range tc.probabilities {
				if got := float64(counts[rank]) / draws; math.Abs(got-want) > .01 {
					t.Errorf("rank %d probability = %.5f, want %.5f (+/- .01)", rank+1, got, want)
				}
			}
		})
	}
}

func TestZipfSingletonAndExtremeExponent(t *testing.T) {
	r := rand.New(rand.NewSource(91))

	for _, exponent := range []float64{0, .5, 1, 2, math.MaxFloat64} {
		z := newZipf(1, exponent)
		for i := 0; i < 1000; i++ {
			if got := z.sample(r); got != 0 {
				t.Fatalf("singleton exponent %g sampled %d", exponent, got)
			}
		}
	}

	z := newZipf(1000, math.MaxFloat64)
	for i := 0; i < 1000; i++ {
		if got := z.sample(r); got != 0 {
			t.Fatalf("extreme exponent sampled %d, want rank 1", got)
		}
	}
}
