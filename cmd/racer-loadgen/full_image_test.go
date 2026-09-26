// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestFullImageDiagnostic is an opt-in, finite in-cluster batch. It uses the real
// loadgen puller, including SHA-256 and length checks on all ten image objects.
func TestFullImageDiagnostic(t *testing.T) {
	target := os.Getenv("FULL_IMAGE_TARGET")
	if target == "" {
		t.Skip("set FULL_IMAGE_TARGET for the in-cluster full-image regression")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
	defer cancel()

	seed := os.Getenv("FULL_IMAGE_SEED")
	if seed == "" {
		seed = "benchmark-v1"
	}

	img, err := newImage(ctx, imageOptions{Repository: "benchmark/image", Layers: 8, LayerBytes: 64 << 20, Jitter: 0.2, Seed: seed})
	require.NoError(t, err)
	l, err := net.Listen("tcp", ":8080")
	require.NoError(t, err)

	srv := &http.Server{Handler: img.handler(), ReadHeaderTimeout: 5 * time.Second}

	t.Cleanup(func() { require.NoError(t, srv.Close()) })

	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			t.Errorf("serve origin: %v", err)
		}
	}()

	concurrency, err := strconv.Atoi(os.Getenv("FULL_IMAGE_CONCURRENCY"))
	require.NoError(t, err)
	require.Greater(t, concurrency, 0)
	require.LessOrEqual(t, concurrency, 64)

	opts := pullTestOptions(target)
	opts.Namespace = "loadgen.invalid"
	opts.Concurrency, opts.LayerConcurrency, opts.Timeout = concurrency, 4, 90*time.Second
	p, metrics := pullTestNew(t, img, opts)
	require.True(t, waitPullDelay(ctx, 5*time.Second), "origin discovery delay")

	imageBytes := img.Manifest.Size + img.Config.Size
	for _, layer := range img.Layers {
		imageBytes += layer.Size
	}

	fmt.Printf("FIXTURE digest=%s layers=%d bytes=%d seed=%s concurrency=%d\n", img.Manifest.Digest, len(img.Layers), imageBytes, seed, concurrency)

	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Go(func() {
			start := time.Now()
			err := p.pull(ctx)
			fmt.Printf("IMAGE worker=%d elapsed=%s error=%v\n", i, time.Since(start), err)
		})
	}

	wg.Wait()
	fmt.Printf("TOTAL success=%.0f error=%.0f canceled=%.0f bytes=%.0f\n", testutil.ToFloat64(metrics.pulls.WithLabelValues("success")), testutil.ToFloat64(metrics.pulls.WithLabelValues("error")), testutil.ToFloat64(metrics.pulls.WithLabelValues("canceled")), testutil.ToFloat64(metrics.receivedBytes))
	require.Equal(t, float64(concurrency), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")), "incomplete full images")
	require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("error")))
	require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("canceled")))
	require.Zero(t, testutil.ToFloat64(metrics.inFlight))
	require.Equal(t, float64(concurrency*8), testutil.ToFloat64(metrics.requests.WithLabelValues("layer", "success")))
	require.Equal(t, float64(int64(concurrency)*imageBytes), testutil.ToFloat64(metrics.receivedBytes))
}
