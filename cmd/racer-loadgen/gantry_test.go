// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func TestGantryIntegration(t *testing.T) {
	// Exercise the real HTTP mirror, SDK protocol, Gantry adapter, and HTTP
	// origin client. Only Racer is replaced by its noncaching protocol fake;
	// this does not test Racer processes, cache hits, or peer distribution.
	const page = int64(racersdk.PageSize)

	catalog, err := newCatalog(t.Context(), imageOptions{
		Layers: 1, LayerBytes: 2*page + 4099, Seed: "gantry-integration", Repository: "benchmark/nested-image",
	}, 3)
	require.NoError(t, err)

	img := catalog.images[0]

	layer := img.Layers[0]
	layerPath := "/v2/" + img.repository + "/blobs/" + layer.Digest.String()
	registry := catalog.handler()

	var (
		rejectContinuation atomic.Bool
		corrupt            atomic.Bool
	)

	var mu sync.Mutex

	ranges := make(map[string]int)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == layerPath {
			rangeHeader := r.Header.Get("Range")

			mu.Lock()
			ranges[rangeHeader]++
			mu.Unlock()

			if rejectContinuation.Load() && rangeHeader != fmt.Sprintf("bytes=0-%d", page-1) {
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

	client := racersdktest.NewClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		callbacks.Add(1)

		metadata, body, err := callback(ctx, req)
		if err == nil && body != nil && corrupt.Load() {
			var first [1]byte
			if _, readErr := io.ReadFull(body, first[:]); readErr != nil {
				return metadata, body, readErr
			}

			first[0] ^= 0xff
			body = &corruptBody{Reader: io.MultiReader(bytes.NewReader(first[:]), body), Closer: body}
		}

		return metadata, body, err
	})

	server := httptest.NewServer(mirror.New(cfg, nil, upstream, mirror.WithContentBackend(gantryracer.NewHandler(client, upstream, nil))).Handler())
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
		corrupt.Store(false)

		return pullTestNew(t, img, opts)
	}

	assertRanges := func(t *testing.T, first, second, third int) {
		t.Helper()

		want := make(map[string]int)

		for i, count := range []int{first, second, third} {
			if count == 0 {
				continue
			}

			header := fmt.Sprintf("bytes=%d-%d", int64(i)*page, int64(i+1)*page-1)

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
		assembled := sha256.New()
		_, err = io.Copy(assembled, io.NewSectionReader(img.blobs[layer.Digest].data, 0, offset))
		require.NoError(t, err)
		n, actual, err := p.readBody(io.TeeReader(resp.Body, assembled))
		require.NoError(t, err)
		require.Equal(t, layer.Size-offset, n)
		require.Equal(t, layer.Digest.String(), fmt.Sprintf("sha256:%x", assembled.Sum(nil)), "consumer must verify the complete resumed object")

		want := sha256.New()
		_, err = io.Copy(want, io.NewSectionReader(img.blobs[layer.Digest].data, offset, n))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("sha256:%x", want.Sum(nil)), actual)
		require.Equal(t, float64(n), testutil.ToFloat64(metrics.receivedBytes)-before)
		// The noncaching fake validates the subscription pin with an origin HEAD
		// after Stat, then fetches only the two selected pages, never page zero.
		require.Equal(t, int64(4), callbacks.Load())
		assertRanges(t, 0, 1, 1)
	})

	t.Run("verified catalog through mirror", func(t *testing.T) {
		p, metrics := newPull(t)

		var want int64

		for _, image := range catalog.images {
			require.NoError(t, p.pullImage(t.Context(), image))
			want += image.Manifest.Size + image.Config.Size + image.Layers[0].Size
		}

		require.Equal(t, float64(want), testutil.ToFloat64(metrics.verifiedBytes))
		require.Equal(t, float64(3), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
	})

	t.Run("body-free HEAD", func(t *testing.T) {
		p, _ := newPull(t)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+layerPath+"?ns="+opts.Namespace, nil)
		require.NoError(t, err)
		resp, err := p.client.Do(req)
		require.NoError(t, err)

		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Empty(t, body)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, layer.Size, resp.ContentLength)
		require.Equal(t, int64(1), callbacks.Load())
		assertRanges(t, 0, 0, 0)
	})

	t.Run("consumer rejects transported corruption", func(t *testing.T) {
		p, metrics := newPull(t)
		require.True(t, p.opts.Verify, "consumer verification must remain enabled")
		corrupt.Store(true)

		err := p.pull(t.Context())
		require.ErrorContains(t, err, "digest mismatch")
		require.Equal(t, float64(img.Manifest.Size), testutil.ToFloat64(metrics.receivedBytes), "mirror must deliver the complete corrupt object")
		require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
		require.Equal(t, float64(1), testutil.ToFloat64(metrics.pulls.WithLabelValues("error")))
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

type corruptBody struct {
	io.Reader
	io.Closer
}

func TestGantryGenericBlobs(t *testing.T) {
	// The mirror and Gantry origin adapter are real; the SDK fake is noncaching.
	catalog, err := newBlobCatalog(t.Context(), "benchmark/blobs", "generic-gantry", 2, int64(racersdk.PageSize)+71)
	require.NoError(t, err)

	upstreamServer := httptest.NewServer(catalog.handler())
	t.Cleanup(upstreamServer.Close)
	cfg := &config.Config{RacerEnabled: true, UpstreamRegistries: []config.UpstreamRegistry{{Name: "loadgen.invalid", Endpoint: upstreamServer.URL}}}
	upstream, err := origin.New(cfg)
	require.NoError(t, err)
	client := racersdktest.NewClient(t, gantryracer.Origin(cfg, upstream))

	server := httptest.NewServer(mirror.New(cfg, nil, upstream, mirror.WithContentBackend(gantryracer.NewHandler(client, upstream, nil))).Handler())
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.Namespace = "loadgen.invalid"
	p, metrics := pullTestNew(t, &syntheticImage{repository: catalog.repository}, opts)
	p.opts.DiagnoseIntegrity = true
	require.NoError(t, p.configureDiagnostics(catalog))

	for _, batch := range catalog.batches {
		require.NoError(t, p.pullBatch(t.Context(), batch))
	}

	require.Equal(t, float64(2*(int64(racersdk.PageSize)+71)), testutil.ToFloat64(metrics.verifiedBytes))
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.requests.WithLabelValues("blob", "success")))
}
