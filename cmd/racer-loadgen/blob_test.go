// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func TestBlobOptions(t *testing.T) {
	opts, err := parseOptions([]string{"--backend=uds", "--cache=blob-test", "--catalog-blobs=3", "--blob-bytes=123", "--blob-concurrency=7"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "uds", opts.pull.Backend)
	require.Equal(t, "blob-test", opts.pull.Cache)
	require.Equal(t, 3, opts.catalogBlobs)
	require.Equal(t, int64(123), opts.blobBytes)
	require.Equal(t, 7, opts.pull.LayerConcurrency)

	for _, args := range [][]string{
		{"--backend=other"},
		{"--backend=uds"},
		{"--backend=uds", "--cache=../bad"},
		{"--catalog-blobs=0"},
		{"--catalog-blobs=513"},
		{"--catalog-blobs=1", "--blob-bytes=0"},
		{"--blob-bytes=12"},
		{"--catalog-blobs=1", "--jitter=0"},
		{"--catalog-blobs=1", "--layers=2"},
		{"--catalog-blobs=1", "--layer-bytes=3"},
		{"--catalog-blobs=1", "--catalog-images=1"},
		{"--blob-concurrency=2", "--layer-concurrency=2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := parseOptions(args, io.Discard)
			require.Error(t, err)
		})
	}
}

func TestBlobCatalogExactDeterministic(t *testing.T) {
	a, err := newBlobCatalog(t.Context(), "test/blobs", "same-seed", 3, 12345)
	require.NoError(t, err)
	b, err := newBlobCatalog(t.Context(), "other/repo", "same-seed", 3, 12345)
	require.NoError(t, err)
	require.Len(t, a.images, 0)
	require.Len(t, a.blobs, 3)
	require.Len(t, a.batches, 3)

	for index, batch := range a.batches {
		require.Empty(t, batch.prefix)
		require.Len(t, batch.blobs, 1)
		desc := batch.blobs[0].descriptor
		require.Equal(t, b.batches[index].blobs[0].descriptor, desc)
		require.Equal(t, int64(12345), desc.Size)
		data, err := io.ReadAll(io.NewSectionReader(a.blobs[desc.Digest].data, 0, desc.Size))
		require.NoError(t, err)
		require.Len(t, data, 12345)
		require.Equal(t, desc.Digest, digest.FromBytes(data))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = newBlobCatalog(ctx, "test/blobs", "seed", 1, 1)
	require.ErrorIs(t, err, context.Canceled)

	for _, count := range []int{0, 513} {
		_, err := newBlobCatalog(t.Context(), "test/blobs", "seed", count, 1)
		require.Error(t, err)
	}

	_, err = newBlobCatalog(t.Context(), "test/blobs", "seed", 512, 1)
	require.ErrorContains(t, err, "duplicate content", "do not advertise more distinct blobs than exist")
}

func TestBlobBackendsShareOperations(t *testing.T) {
	for _, workload := range []string{"generic", "oci"} {
		for _, backend := range []string{"gantry", "uds"} {
			t.Run(workload+"/"+backend, func(t *testing.T) {
				catalog, err := newBlobCatalog(t.Context(), "test/blobs", "same-seed", 3, 12345)
				require.NoError(t, err)

				if workload == "oci" {
					catalog = catalogFromImages([]*syntheticImage{pullTestImage(t)})
				}

				server := httptest.NewServer(catalog.handler())
				t.Cleanup(server.Close)
				p, metrics := pullTestNew(t, &syntheticImage{repository: catalog.repository}, pullTestOptions(server.URL))
				p.opts.DiagnoseIntegrity = true
				require.NoError(t, p.configureDiagnostics(catalog))

				p.batches = catalog.batches
				if backend == "uds" {
					origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
					require.NoError(t, err)
					client, cleanup, err := racersdktest.NewClient(origin)
					require.NoError(t, err)
					t.Cleanup(cleanup)

					p.acquire = udsAcquirer(client)
				}

				var total float64

				traversal := p.newTraversal()
				for range len(catalog.batches) {
					batch := p.nextBatch(&traversal)
					require.NoError(t, p.pullBatch(t.Context(), batch))

					for _, blob := range batch.prefix {
						total += float64(blob.descriptor.Size)
					}

					for _, blob := range batch.blobs {
						total += float64(blob.descriptor.Size)
					}
				}

				require.Equal(t, total, testutil.ToFloat64(metrics.verifiedBytes))
				require.Equal(t, total, testutil.ToFloat64(metrics.receivedBytes))
				require.Equal(t, float64(len(catalog.batches)), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
			})
		}
	}
}

func TestSDKOriginPagesPinsAndUnknownKeys(t *testing.T) {
	const size = int64(racersdk.PageSize) + 73

	catalog, err := newBlobCatalog(t.Context(), "test/blob", "pages", 1, size)
	require.NoError(t, err)

	metrics := newMetrics(prometheus.NewRegistry())
	origin, key, err := syntheticOrigin(catalog, metrics)
	require.NoError(t, err)
	client, cleanup, err := racersdktest.NewClient(origin)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	request := racersdk.Request{Key: key}
	metadata, err := client.Stat(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, racersdk.ByteLength(size), metadata.Size)

	desc := catalog.batches[0].blobs[0].descriptor
	require.Equal(t, `"`+desc.Digest.String()+`"`, metadata.ETag.String())
	require.Equal(t, desc.Digest.Encoded(), key.String(), "key is raw SHA-256, not a hash of the digest string")
	value, err := client.Get(t.Context(), request, racersdk.ReadOptions{Offset: racersdk.ByteOffset(racersdk.PageSize), Pin: metadata.ETag})
	require.NoError(t, err)
	data, err := io.ReadAll(value)
	require.NoError(t, err)
	require.NoError(t, value.Close())
	require.Len(t, data, 73)

	expected := make([]byte, 73)
	_, err = catalog.blobs[desc.Digest].data.ReadAt(expected, int64(racersdk.PageSize))
	require.NoError(t, err)
	require.Equal(t, expected, data)

	wrong, err := racersdk.ParseETag(`"wrong"`)
	require.NoError(t, err)
	_, err = client.Get(t.Context(), request, racersdk.ReadOptions{Pin: wrong})

	var typed *racersdk.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, racersdk.ErrorVersionUnavailable, typed.Kind())
	_, err = client.Stat(t.Context(), racersdk.Request{})
	require.ErrorAs(t, err, &typed)
	require.Equal(t, racersdk.ErrorNotFound, typed.Kind())
	require.Equal(t, float64(73), testutil.ToFloat64(metrics.originBytes))
}

type corruptBlobBody struct{ io.ReadCloser }

func (b corruptBlobBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		p[0] ^= 1
	}

	return n, err
}

func TestSDKBlobDiagnosticsAndFailure(t *testing.T) {
	catalog, err := newBlobCatalog(t.Context(), "test/blob", "bad-data", 1, 8192)
	require.NoError(t, err)
	origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)
	client, cleanup, err := racersdktest.NewClient(func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		metadata, body, err := origin(ctx, request)
		if body != nil {
			body = corruptBlobBody{body}
		}

		return metadata, body, err
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
	p.acquire = udsAcquirer(client)
	p.opts.DiagnoseIntegrity = true
	require.NoError(t, p.configureDiagnostics(catalog))
	err = p.pullBatch(t.Context(), catalog.batches[0])

	var failure *pullFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, failureDigest, failure.reason)
	require.NotNil(t, failure.integrity.pages)
	require.Equal(t, int64(0), failure.integrity.pages.firstMismatch)
	require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
	require.Equal(t, float64(8192), testutil.ToFloat64(metrics.receivedBytes))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.pullFailures.WithLabelValues("digest_mismatch")))
}

func TestBlobStreamMustFinishAndMatchMetadata(t *testing.T) {
	data := []byte("complete")
	desc := byteDescriptor("application/octet-stream", data)

	for _, mode := range []string{"metadata", "short", "long", "late-error", "close-error"} {
		for _, diagnostic := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/diagnostic=%v", mode, diagnostic), func(t *testing.T) {
				p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
				p.opts.DiagnoseIntegrity = diagnostic
				p.expected = map[digest.Digest]imageBlob{desc.Digest: {descriptor: desc, data: bytes.NewReader(data)}}

				var closed atomic.Int32

				p.acquire = func(context.Context, string, ocispec.Descriptor) (blobResponse, error) {
					size := desc.Size
					payload := data
					end := io.EOF

					var closeErr error

					switch mode {
					case "metadata":
						size++
					case "short":
						payload = data[:len(data)-1]
					case "long":
						payload = append(append([]byte(nil), data...), 'x')
					case "late-error":
						end = fmt.Errorf("unclean end: %w", io.EOF)
					case "close-error":
						closeErr = errors.New("close failed")
					}

					return blobResponse{success: true, totalSize: &size, body: &blobTestBody{Reader: &diagnosticChunks{payload, 3, end}, closed: &closed, closeErr: closeErr}}, nil
				}
				require.Error(t, p.pullBatch(t.Context(), blobBatch{blobs: []blobRequest{{"blob", desc}}}))
				require.Equal(t, int32(1), closed.Load())
				require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
			})
		}
	}
}

type blobTestBody struct {
	io.Reader
	closed   *atomic.Int32
	closeErr error
}

func (b *blobTestBody) Close() error { b.closed.Add(1); return b.closeErr }

func TestSDKAdmissionAndIndependentTarget(t *testing.T) {
	opts := pullTestOptions("not a URL")
	opts.Backend, opts.Cache, opts.Concurrency, opts.LayerConcurrency = "uds", "test", 100, 8
	config, err := sdkClientConfig(opts, opts.Concurrency)
	require.NoError(t, err)
	require.Equal(t, 800, config.MaxConnections)
	config, err = sdkClientConfig(opts, maxLiveConcurrency)
	require.NoError(t, err)
	require.Equal(t, 2048, config.MaxConnections)

	p, err := newPuller(&syntheticImage{}, opts, pullTestMetrics())
	require.NoError(t, err, "NewClient is valid without dialing a Racer or Gantry endpoint")
	p.close()
}

func TestUDSRunOriginOnlyReadinessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			opts := loadgenTestOptions(t)
			loadgenTestAddresses(t, &opts)
			opts.pull.Backend, opts.pull.Cache, opts.pull.Concurrency = "uds", "test", 0
			opts.catalogBlobs, opts.blobBytes = 2, 100
			opts.listen = "invalid HTTP listener must not be used"

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			failure := make(chan func(error), 1)
			done := make(chan error, 1)

			go func() {
				done <- runWithOriginStarter(ctx, opts, func(_ context.Context, _ context.Context, config racersdk.OriginConfig, _ racersdk.Origin, _ racersdk.Key, failed func(error)) (func(), error) {
					if !config.RecoverStaleSocket {
						return nil, errors.New("ownership required")
					}

					close(entered)
					<-release

					failure <- failed

					return func() { close(stopped) }, nil
				})
			}()

			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("origin did not start at zero concurrency")
			}

			client := &http.Client{Timeout: time.Second}
			status := func() int {
				response, err := client.Get("http://" + opts.metricsListen + "/readyz")
				if err != nil {
					return 0
				}
				defer response.Body.Close()

				return response.StatusCode
			}
			require.Equal(t, http.StatusServiceUnavailable, status())
			close(release)
			require.Eventually(t, func() bool { return status() == http.StatusOK }, 3*time.Second, time.Millisecond)

			if fail {
				(<-failure)(errors.New("origin failed"))
			} else {
				cancel()
			}

			select {
			case err := <-done:
				if fail {
					require.ErrorContains(t, err, "origin failed")
				} else {
					require.NoError(t, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("run did not stop")
			}

			select {
			case <-stopped:
			default:
				t.Fatal("origin not joined")
			}
		})
	}
}

func TestSDKBlobCancellationAndTimeout(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			catalog, err := newBlobCatalog(t.Context(), "test/blob", "cancel", 1, 1024)
			require.NoError(t, err)

			entered := make(chan struct{}, 1)
			returned := make(chan struct{}, 1)
			client, cleanup, err := racersdktest.NewClient(func(ctx context.Context, _ racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				entered <- struct{}{}

				<-ctx.Done()

				returned <- struct{}{}

				return racersdk.Metadata{}, nil, ctx.Err()
			})
			require.NoError(t, err)
			t.Cleanup(cleanup)

			opts := pullTestOptions("http://unused")
			if !canceled {
				opts.Timeout = 100 * time.Millisecond
			}

			p, metrics := pullTestNew(t, &syntheticImage{}, opts)
			p.acquire = udsAcquirer(client)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)

			go func() { done <- p.pullBatch(ctx, catalog.batches[0]) }()

			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("Get did not reach origin")
			}

			if canceled {
				cancel()
			}

			select {
			case err := <-done:
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			case <-time.After(time.Second):
				t.Fatal("operation did not stop")
			}

			require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
			require.Zero(t, testutil.ToFloat64(metrics.inFlight))
			cleanup()

			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("origin callback not canceled")
			}
		})
	}
}

func TestBlobTraversalAndLiveAdmission(t *testing.T) {
	catalog, err := newBlobCatalog(t.Context(), "test/blob", "admission", 3, 1024)
	require.NoError(t, err)
	p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
	p.batches = catalog.batches
	traversal := p.newTraversal()

	seen := make(map[digest.Digest]bool)
	for range len(catalog.batches) {
		seen[p.nextBatch(&traversal).blobs[0].descriptor.Digest] = true
	}

	require.Len(t, seen, 3)

	p.opts.Profile = profileZipf
	p.randomFloat64 = func() float64 { return 0 }

	traversal = p.newTraversal()
	for range 10 {
		require.Equal(t, catalog.batches[0], p.nextBatch(&traversal))
	}

	origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)
	client, cleanup, err := racersdktest.NewClient(origin)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	p.acquire = udsAcquirer(client)
	p.opts.ConcurrencyFile = filepath.Join(t.TempDir(), "concurrency")
	p.opts.Interval = 5 * time.Millisecond
	replaceConcurrency(t, p.opts.ConcurrencyFile, "0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() { p.runLive(ctx, time.Millisecond); close(done) }()

	t.Cleanup(func() { cancel(); <-done })
	require.Never(t, func() bool { return testutil.ToFloat64(metrics.pulls.WithLabelValues("success")) != 0 }, 20*time.Millisecond, time.Millisecond)
	replaceConcurrency(t, p.opts.ConcurrencyFile, "2")
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.pulls.WithLabelValues("success")) >= 2 }, time.Second, time.Millisecond)
	replaceConcurrency(t, p.opts.ConcurrencyFile, "0")
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.appliedConcurrency) == 0 && testutil.ToFloat64(metrics.inFlight) == 0
	}, time.Second, time.Millisecond)

	before := testutil.ToFloat64(metrics.pulls.WithLabelValues("success"))

	require.Never(t, func() bool { return testutil.ToFloat64(metrics.pulls.WithLabelValues("success")) != before }, 20*time.Millisecond, time.Millisecond)
}

func TestSDKBlobNodeCapAdmission(t *testing.T) {
	catalog, err := newBlobCatalog(t.Context(), "test/blob", "caps", 1, 1024)
	require.NoError(t, err)
	p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
	p.batches = catalog.batches
	origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)
	client, cleanup, err := racersdktest.NewClient(origin)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	p.acquire = udsAcquirer(client)
	root := t.TempDir()
	p.opts.ConcurrencyFile, p.opts.NodeCapsFile, p.opts.NodeName = filepath.Join(root, "concurrency"), filepath.Join(root, "caps"), "node-a"
	p.opts.Interval = time.Hour

	for _, name := range []string{"concurrency", "caps"} {
		require.NoError(t, os.Symlink(filepath.Join("..data", name), filepath.Join(root, name)))
	}

	capProjection(t, root, "initial", "8", `{"version":1,"caps":{"node-a":0}}`)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() { p.runLive(ctx, time.Millisecond); close(done) }()

	t.Cleanup(func() { cancel(); <-done })
	require.Never(t, func() bool { return testutil.ToFloat64(metrics.pulls.WithLabelValues("success")) > 0 }, 20*time.Millisecond, time.Millisecond)
	capProjection(t, root, "admit", "8", `{"version":1,"caps":{"node-a":2}}`)
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.pulls.WithLabelValues("success")) == 2 }, time.Second, time.Millisecond)
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.appliedConcurrency))
	capProjection(t, root, "pause", "0", "broken")
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) == 0 }, time.Second, time.Millisecond)
	capProjection(t, root, "invalid-resume", "8", "missing")
	require.Never(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) != 0 }, 20*time.Millisecond, time.Millisecond)
}
