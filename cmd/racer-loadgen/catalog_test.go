// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestCatalogIdenticalOriginsAndCompatibility(t *testing.T) {
	opts := testImageOptions()
	legacy, err := newImage(t.Context(), opts)
	require.NoError(t, err)
	first, err := newCatalog(t.Context(), opts, 3)
	require.NoError(t, err)
	second, err := newCatalog(t.Context(), opts, 4)
	require.NoError(t, err)
	require.Equal(t, legacy.manifest, first.images[0].manifest)
	require.Equal(t, legacy.Layers, first.images[0].Layers)

	seen := make(map[digest.Digest]bool)

	for index, img := range first.images {
		require.Equal(t, img.manifest, second.images[index].manifest, "catalog growth preserves existing content")

		for _, layer := range img.Layers {
			require.False(t, seen[layer.Digest], "layers must not be shared across catalog entries")
			seen[layer.Digest] = true
			require.Equal(t, readImageBlob(t, img, layer), readImageBlob(t, second.images[index], layer))
			_, virtual := img.blobs[layer.Digest].data.(*virtualLayer)
			require.True(t, virtual, "catalog must retain virtual generation")
		}
	}

	for _, catalog := range []*blobCatalog{first, second} {
		handler := catalog.handler()
		prefix := "/v2/" + opts.Repository

		for index, img := range first.images {
			for _, ref := range []string{fmt.Sprintf("image-%06d", index), img.Manifest.Digest.String()} {
				response := registryRequest(handler, http.MethodGet, prefix+"/manifests/"+ref, "")
				require.Equal(t, http.StatusOK, response.Code)
				require.Equal(t, img.manifest, response.Body.Bytes())
			}

			for _, layer := range img.Layers {
				path := prefix + "/blobs/" + layer.Digest.String()
				response := registryRequest(handler, http.MethodGet, path, "bytes=497-529")
				require.Equal(t, http.StatusPartialContent, response.Code)
				require.Equal(t, readImageBlob(t, img, layer)[497:530], response.Body.Bytes())
				require.Equal(t, http.StatusOK, registryRequest(handler, http.MethodHead, path, "").Code)
			}
		}

		require.Equal(t, legacy.manifest, registryRequest(handler, http.MethodGet, prefix+"/manifests/latest", "").Body.Bytes())

		for _, ref := range []string{"image-000512", "image--1", "image-1", "missing"} {
			require.Equal(t, http.StatusNotFound, registryRequest(handler, http.MethodGet, prefix+"/manifests/"+ref, "").Code)
		}
	}
}

func TestCatalogBoundsAndCancellation(t *testing.T) {
	opts := testImageOptions()
	for _, count := range []int{-1, 0, maxCatalogImages + 1} {
		catalog, err := newCatalog(t.Context(), opts, count)
		require.Error(t, err)
		require.Nil(t, catalog)
	}

	opts.Layers, opts.LayerBytes = 1, 32
	catalog, err := newCatalog(t.Context(), opts, maxCatalogImages)
	require.NoError(t, err)
	require.Len(t, catalog.images, maxCatalogImages)
	require.Len(t, catalog.blobs, maxCatalogImages*2)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	catalog, err = newCatalog(ctx, opts, 2)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, catalog)

	opts.Layers = 0
	catalog, err = newCatalog(t.Context(), opts, 2)
	require.Error(t, err)
	require.Nil(t, catalog)
}

func TestCatalogTinyPayloadDeduplication(t *testing.T) {
	// One payload byte has only 256 possible contents. Digest-addressed blobs
	// can legitimately coincide at tiny sizes; every catalog tag must still work.
	opts := testImageOptions()
	opts.Layers, opts.LayerBytes = 1, 1
	catalog, err := newCatalog(t.Context(), opts, 257)
	require.NoError(t, err)
	require.LessOrEqual(t, len(catalog.blobs), 512)

	for index, img := range catalog.images {
		response := registryRequest(catalog.handler(), http.MethodGet,
			fmt.Sprintf("/v2/%s/manifests/image-%06d", opts.Repository, index), "")
		require.Equal(t, img.manifest, response.Body.Bytes())
		readImageBlob(t, img, img.Layers[0])
	}
}

func TestCatalogTraversalCompletePasses(t *testing.T) {
	for _, count := range []int{1, 7, 256, 512} {
		images := make([]*syntheticImage, count)
		for i := range images {
			images[i] = &syntheticImage{}
		}

		var traversal catalogTraversal

		for range 3 {
			seen := make(map[*syntheticImage]bool)

			for range count {
				img := traversal.nextImage(images)
				require.False(t, seen[img])
				seen[img] = true
			}

			require.Len(t, seen, count)
		}
	}
}

func TestCatalogWorkerTraversalAndVerifiedBytes(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			catalog, err := newCatalog(t.Context(), testImageOptions(), 5)
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
			opts.Interval, opts.RetryDelay = time.Millisecond, time.Millisecond
			p, metrics := pullTestNew(t, catalog.images[0], opts)
			p.batches = catalog.batches
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})

			go func() { p.run(ctx); close(done) }()

			t.Cleanup(func() { cancel(); <-done })
			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()

				return len(refs) >= 10
			}, 5*time.Second, time.Millisecond)
			cancel()
			<-done

			for pass := range 2 {
				seen := make(map[string]bool)
				for _, ref := range refs[pass*5 : (pass+1)*5] {
					seen[ref] = true
				}

				require.Len(t, seen, 5, "failures must also advance traversal")
			}

			if fail {
				require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
				return
			}
			// A separate full pass verifies exact heterogeneous byte accounting.
			p, metrics = pullTestNew(t, catalog.images[0], opts)

			var want int64

			sizes := make(map[int64]bool)

			for _, img := range catalog.images {
				require.NoError(t, p.pullImage(t.Context(), img))

				size := img.Manifest.Size + img.Config.Size
				for _, layer := range img.Layers {
					size += layer.Size
				}

				sizes[size] = true
				want += size
			}

			require.Greater(t, len(sizes), 1)
			require.Equal(t, float64(want), testutil.ToFloat64(metrics.verifiedBytes))
			require.Equal(t, float64(want), testutil.ToFloat64(metrics.receivedBytes))
		})
	}
}

func TestRunCatalogStartupDeadline(t *testing.T) {
	opts := loadgenTestOptions(t)
	loadgenTestAddresses(t, &opts)
	opts.catalogImages = 256
	opts.image.LayerBytes = 1 << 40
	opts.startupTimeout = 100 * time.Millisecond

	running := startLoadgenTest(t, opts)
	select {
	case <-running.done:
		require.ErrorIs(t, running.err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("catalog initialization ignored deadline")
	}

	assertLoadgenStopped(t, loadgenTestClient(t), opts)
}

func TestRunCatalogReadyServesEveryImage(t *testing.T) {
	opts := loadgenTestOptions(t)
	loadgenTestAddresses(t, &opts)
	opts.catalogImages = 3
	opts.pull.Concurrency = 0
	running := startLoadgenTest(t, opts)
	client := loadgenTestClient(t)
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)

	for index := range opts.catalogImages {
		status, _, err := loadgenTestGet(client, fmt.Sprintf("http://%s/v2/%s/manifests/image-%06d", opts.listen, opts.image.Repository, index))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
	}

	running.cancel()
	running.wait(t)
}
