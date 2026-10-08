// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestZipfFiniteDistribution(t *testing.T) {
	for _, exponent := range []float64{0.5, 1, defaultZipfExponent, 3} {
		t.Run(fmt.Sprint(exponent), func(t *testing.T) {
			const count = 8

			cdf := newZipfCDF(count, exponent)
			require.Equal(t, 1.0, cdf[count-1])

			var sum float64
			for rank := 1; rank <= count; rank++ {
				sum += 1 / math.Pow(float64(rank), exponent)
			}

			previous, previousWeight := 0.0, math.Inf(1)
			for index, upper := range cdf {
				weight := upper - previous
				require.InDelta(t, 1/(math.Pow(float64(index+1), exponent)*sum), weight, 1e-14)
				require.Positive(t, weight)
				require.Less(t, weight, previousWeight, "lower indices must be hotter")
				require.Equal(t, index, zipfIndex(cdf, previous), "CDF boundaries belong to the next bin")
				require.Equal(t, index, zipfIndex(cdf, (previous+upper)/2))
				previous, previousWeight = upper, weight
			}

			// Fixed-seed draws check the sampler against the expected finite law.
			images := make([]*syntheticImage, count)
			indices := make(map[*syntheticImage]int)

			for index := range images {
				images[index] = &syntheticImage{}
				indices[images[index]] = index
			}

			rng := rand.New(rand.NewPCG(17, 42))
			traversal := catalogTraversal{zipfCDF: cdf, randomFloat64: rng.Float64}
			counts := make([]int, count)

			const draws = 100000
			for range draws {
				img := traversal.nextImage(images)
				index, ok := indices[img]
				require.True(t, ok)

				counts[index]++
			}

			previous = 0
			for index, upper := range cdf {
				require.InDelta(t, upper-previous, float64(counts[index])/draws, 0.005)
				previous = upper
			}
		})
	}
}

func TestZipfBoundsAndExtremeExponents(t *testing.T) {
	for _, count := range []int{1, 7, maxCatalogImages} {
		for _, exponent := range []float64{math.SmallestNonzeroFloat64, 0.5, 1, 1.2, math.MaxFloat64} {
			cdf := newZipfCDF(count, exponent)
			require.Len(t, cdf, count)

			previous := 0.0

			for _, upper := range cdf {
				require.False(t, math.IsNaN(upper) || math.IsInf(upper, 0))
				require.GreaterOrEqual(t, upper, previous)
				require.LessOrEqual(t, upper, 1.0)
				previous = upper
			}

			for _, draw := range []float64{0, math.SmallestNonzeroFloat64, 0.25, 0.5, math.Nextafter(1, 0)} {
				index := zipfIndex(cdf, draw)
				require.GreaterOrEqual(t, index, 0)
				require.Less(t, index, count)

				if count == 1 || exponent == math.MaxFloat64 {
					require.Zero(t, index)
				}
			}
		}
	}
}

func TestPullProfileValidation(t *testing.T) {
	for _, profile := range []string{"", profileShuffle, profileZipf, "unknown"} {
		for _, exponent := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 0.5, 1, 1.2} {
			opts := pullTestOptions("http://localhost")
			opts.Profile, opts.ZipfExponent = profile, exponent

			p, err := newPuller(&syntheticImage{}, opts, pullTestMetrics())
			if profile == "unknown" || math.IsNaN(exponent) || math.IsInf(exponent, 0) || exponent <= 0 {
				require.Error(t, err)
				require.Nil(t, p)

				continue
			}

			require.NoError(t, err)
			p.transport.CloseIdleConnections()

			if profile == "" {
				require.Equal(t, profileShuffle, p.opts.Profile)
			}
		}
	}
}

func TestRunRejectsProfileBeforeStartup(t *testing.T) {
	for _, test := range []struct {
		profile  string
		exponent float64
		want     string
	}{
		{"unknown", 1.2, "profile must be"},
		{profileZipf, 0, "zipf-exponent"},
		{profileShuffle, math.NaN(), "zipf-exponent"},
	} {
		opts := loadgenTestOptions(t)
		opts.pull.Profile, opts.pull.ZipfExponent = test.profile, test.exponent
		// Invalid listener and image settings prove selection validation comes
		// before binding listeners or generating even the first image.
		opts.listen = "invalid address"
		opts.image.Layers = 0
		require.ErrorContains(t, run(t.Context(), opts), test.want)
	}
}

func TestZipfWorkerDrawsAfterSuccessAndFailure(t *testing.T) {
	for _, mode := range []string{"fixed", "concurrency-file", "node-caps"} {
		t.Run(mode, func(t *testing.T) {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
					catalog, err := newCatalog(t.Context(), testImageOptions(), 3)
					require.NoError(t, err)

					handler := catalog.handler()

					var (
						mu   sync.Mutex
						refs []string
					)

					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if _, ref, ok := strings.Cut(r.URL.Path, "/manifests/"); ok {
							mu.Lock()

							refs = append(refs, ref)
							mu.Unlock()
						}

						if fail {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}

						handler.ServeHTTP(w, r)
					}))
					t.Cleanup(server.Close)
					opts := pullTestOptions(server.URL)
					opts.Profile, opts.ZipfExponent = profileZipf, 1
					opts.RetryDelay = time.Millisecond

					if mode != "fixed" {
						dir := t.TempDir()

						opts.ConcurrencyFile = filepath.Join(dir, "concurrency")
						if mode == "node-caps" {
							opts.NodeCapsFile, opts.NodeName = filepath.Join(dir, "caps"), "node-a"
							capProjection(t, dir, "start", "8", `{"version":1,"caps":{"node-a":1}}`)
						} else {
							replaceConcurrency(t, opts.ConcurrencyFile, "1")
						}
					}

					p, metrics := pullTestNew(t, catalog.images[0], opts)
					p.images = catalog.images

					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					// For exponent 1 and three entries the CDF is [6/11,9/11,1].
					// Repeated hot draws prove replacement; following cold draws prove
					// failure does not stick to the previous image or reshuffle ranks.
					draws := []float64{0, 0.1, 0.99, 0.7, 0.2, 0.9}
					wantIndices := []int{0, 0, 2, 1, 0, 2}
					next := 0
					p.randomFloat64 = func() float64 {
						if next == len(draws) {
							cancel()
							return 0
						}

						draw := draws[next]
						next++

						return draw
					}
					p.run(ctx)
					require.Equal(t, len(draws), next)

					var (
						wantRefs  []string
						wantBytes int64
					)

					for _, index := range wantIndices {
						img := catalog.images[index]
						wantRefs = append(wantRefs, img.Manifest.Digest.String())

						wantBytes += img.Manifest.Size + img.Config.Size
						for _, layer := range img.Layers {
							wantBytes += layer.Size
						}
					}

					mu.Lock()

					gotRefs := append([]string(nil), refs...)
					mu.Unlock()
					require.Equal(t, wantRefs, gotRefs)

					result := "success"
					if fail {
						result, wantBytes = "error", 0
					}

					require.Equal(t, float64(len(draws)), testutil.ToFloat64(metrics.pulls.WithLabelValues(result)))
					require.Equal(t, float64(wantBytes), testutil.ToFloat64(metrics.verifiedBytes))
				})
			}
		})
	}
}
