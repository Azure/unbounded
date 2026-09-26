// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestGantryIntegration(t *testing.T) {
	// Exercise the real HTTP mirror, SDK protocol, Gantry adapter, and HTTP
	// origin client. Only Racer is replaced by its noncaching protocol fake;
	// this does not test Racer processes, cache hits, or peer distribution.
	const page = int64(racersdk.PageSize)

	img, err := newImage(t.Context(), imageOptions{
		Layers: 1, LayerBytes: 2*page + 4099, Seed: "gantry-integration", Repository: "benchmark/nested-image",
	})
	require.NoError(t, err)

	layer := img.Layers[0]
	layerPath := "/v2/" + img.repository + "/blobs/" + layer.Digest.String()
	registry := img.handler()

	var rejectContinuation atomic.Bool

	var mu sync.Mutex

	ranges := make(map[string]int)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == layerPath {
			rangeHeader := r.Header.Get("Range")

			mu.Lock()
			ranges[rangeHeader]++
			mu.Unlock()

			if rejectContinuation.Load() && rangeHeader != "" {
				http.Error(w, "injected origin failure", http.StatusServiceUnavailable)
				return
			}
		}

		registry.ServeHTTP(w, r)
	}))
	t.Cleanup(upstreamServer.Close)

	// Multiple upstreams make ns routing necessary rather than letting a
	// missing namespace silently succeed via Gantry's single-upstream default.
	cfg := &config.Config{
		RacerEnabled: true,
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "loadgen.invalid", Endpoint: upstreamServer.URL},
			{Name: "unused.invalid", Endpoint: upstreamServer.URL + "/wrong-upstream"},
		},
	}
	upstream, err := origin.New(cfg)
	require.NoError(t, err)

	callback := gantryracer.Origin(cfg, upstream)

	var callbacks atomic.Int64

	client, cleanup, err := racersdk.NewFakeClient(func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		callbacks.Add(1)
		return callback(ctx, req)
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)

	server := httptest.NewServer(mirror.New(cfg, nil, upstream, mirror.WithRacer(client)).Handler())
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.Namespace = "loadgen.invalid"
	opts.Timeout = 30 * time.Second
	imageBytes := img.Manifest.Size + img.Config.Size + layer.Size
	newPull := func(t *testing.T) (*puller, *loadMetrics) {
		t.Helper()
		mu.Lock()
		ranges = make(map[string]int)
		mu.Unlock()
		callbacks.Store(0)
		rejectContinuation.Store(false)

		return pullTestNew(t, img, opts)
	}

	assertRanges := func(t *testing.T, first, second, third int) {
		t.Helper()

		want := make(map[string]int)

		for i, count := range []int{first, second, third} {
			if count == 0 {
				continue
			}

			header := ""
			if i != 0 {
				header = fmt.Sprintf("bytes=%d-", int64(i)*page)
			}

			want[header] = count
		}

		mu.Lock()
		defer mu.Unlock()

		require.Equal(t, want, ranges, "unexpected origin range or direct-content fallback")
	}

	t.Run("concurrent verified pulls", func(t *testing.T) {
		p, metrics := newPull(t)
		results := make(chan error, 2)

		for range 2 {
			go func() { results <- p.pull(t.Context()) }()
		}

		// Join both requests before asserting so cleanup never races a pull.
		first, second := <-results, <-results
		require.NoError(t, first)
		require.NoError(t, second)
		require.Equal(t, float64(2*imageBytes), testutil.ToFloat64(metrics.receivedBytes))
		require.Equal(t, float64(2), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
		require.Zero(t, testutil.ToFloat64(metrics.inFlight))
		require.Equal(t, int64(10), callbacks.Load(), "each pull needs manifest, config, and three layer pages")
		assertRanges(t, 2, 2, 2)
	})

	t.Run("resume beyond first page", func(t *testing.T) {
		p, metrics := newPull(t)
		// Both the offset and returned suffix exceed 16 MiB; the offset is
		// deliberately unaligned to exercise virtual-layer random access.
		offset := page + 7
		require.Greater(t, layer.Size-offset, page)

		ctx, cancel := context.WithTimeout(t.Context(), opts.Timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+layerPath+"?ns="+opts.Namespace, nil)
		require.NoError(t, err)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		resp, err := p.client.Do(req)
		require.NoError(t, err)

		defer resp.Body.Close()

		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		require.Equal(t, "1", resp.Header.Get("Gantry-Mirrored"))
		require.Equal(t, layer.Digest.String(), resp.Header.Get("Docker-Content-Digest"))
		require.Equal(t, "bytes", resp.Header.Get("Accept-Ranges"))
		require.Equal(t, fmt.Sprintf("bytes %d-%d/%d", offset, layer.Size-1, layer.Size), resp.Header.Get("Content-Range"))
		require.Equal(t, layer.Size-offset, resp.ContentLength)

		before := testutil.ToFloat64(metrics.receivedBytes)
		n, actual, err := p.readBody(resp.Body)
		require.NoError(t, err)
		require.Equal(t, layer.Size-offset, n)

		want := sha256.New()
		_, err = io.Copy(want, io.NewSectionReader(img.blobs[layer.Digest].data, offset, n))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("sha256:%x", want.Sum(nil)), actual)
		require.Equal(t, float64(n), testutil.ToFloat64(metrics.receivedBytes)-before)
		// Gantry verifies the skipped prefix too, so resume still fetches all
		// three pages through the adapter instead of bypassing Racer.
		require.Equal(t, int64(3), callbacks.Load())
		assertRanges(t, 1, 1, 1)
	})

	t.Run("origin continuation failure reaches puller", func(t *testing.T) {
		p, metrics := newPull(t)

		rejectContinuation.Store(true)

		before := testutil.ToFloat64(metrics.receivedBytes)
		err := p.pull(t.Context())
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Equal(t, float64(img.Manifest.Size+img.Config.Size+page), testutil.ToFloat64(metrics.receivedBytes)-before,
			"count the delivered first page even though continuation failed")
		require.Equal(t, float64(1), testutil.ToFloat64(metrics.pulls.WithLabelValues("error")))
		require.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues("layer", "error")))
		require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
		require.Zero(t, testutil.ToFloat64(metrics.inFlight))
		require.Equal(t, int64(4), callbacks.Load(), "failed page must not be retried or bypassed")
		assertRanges(t, 1, 1, 0)
	})
}
