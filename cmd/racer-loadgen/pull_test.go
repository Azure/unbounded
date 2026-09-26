// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func pullTestImage(t *testing.T) *syntheticImage {
	t.Helper()

	img, err := newImage(t.Context(), imageOptions{
		Layers: 7, LayerBytes: 1024, Seed: "pull-test", Repository: "test/nested-image",
	})
	require.NoError(t, err)

	return img
}

func pullTestOptions(target string) pullOptions {
	return pullOptions{
		Target: target, Namespace: "registry.example:5000/a b&c", Concurrency: 1,
		LayerConcurrency: 2, Timeout: 5 * time.Second, RetryDelay: 200 * time.Millisecond,
		Verify: true,
	}
}

func pullTestMetrics() *loadMetrics {
	return &loadMetrics{
		pulls: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_pulls_total"}, []string{"result"}),
		pullDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "test_pull_duration_seconds",
		}, []string{"result"}),
		inFlight:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_in_flight"}),
		receivedBytes: prometheus.NewCounter(prometheus.CounterOpts{Name: "test_received_bytes_total"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_requests_total",
		}, []string{"kind", "result"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "test_request_duration_seconds",
		}, []string{"kind", "result"}),
	}
}

func pullTestNew(t *testing.T, img *syntheticImage, opts pullOptions) (*puller, *loadMetrics) {
	t.Helper()

	metrics := pullTestMetrics()
	p, err := newPuller(img, opts, metrics)
	require.NoError(t, err)
	t.Cleanup(p.transport.CloseIdleConnections)

	return p, metrics
}

func pullTestHistogramCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()

	observer, err := vec.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)

	metric, ok := observer.(prometheus.Metric)
	require.True(t, ok)

	var value dto.Metric
	require.NoError(t, metric.Write(&value))

	return value.GetHistogram().GetSampleCount()
}

func TestPullCompleteImage(t *testing.T) {
	img := pullTestImage(t)
	registry := img.handler()

	var mu sync.Mutex

	var paths []string

	var connections atomic.Int64

	opts := pullTestOptions("")
	opts.LayerConcurrency = 1
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("ns") != opts.Namespace || len(r.URL.Query()) != 1 {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}

		if r.Header.Get("Range") != "" || r.Header.Get("If-None-Match") != "" {
			t.Errorf("unexpected conditional or partial request: %v", r.Header)
		}

		mu.Lock()

		paths = append(paths, r.URL.Path)
		mu.Unlock()

		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/proxy/base")
		registry.ServeHTTP(w, r)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	opts.Target = server.URL + "/proxy/base/"
	p, metrics := pullTestNew(t, img, opts)

	for range 2 {
		require.NoError(t, p.pull(t.Context()))
	}

	prefix := "/proxy/base/v2/" + img.repository
	expected := []string{prefix + "/manifests/" + img.Manifest.Digest.String(), prefix + "/blobs/" + img.Config.Digest.String()}

	bytes := img.Manifest.Size + img.Config.Size
	for _, layer := range img.Layers {
		expected = append(expected, prefix+"/blobs/"+layer.Digest.String())
		bytes += layer.Size
	}

	mu.Lock()

	actualPaths := append([]string(nil), paths...)
	mu.Unlock()
	require.Equal(t, append(expected, expected...), actualPaths)
	require.Equal(t, int64(1), connections.Load(), "fully drained responses should reuse a connection")
	require.Equal(t, float64(2*bytes), testutil.ToFloat64(metrics.receivedBytes))
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
	require.Zero(t, testutil.ToFloat64(metrics.inFlight))
	require.Equal(t, uint64(2), pullTestHistogramCount(t, metrics.pullDuration, "success"))

	for kind, count := range map[string]int{"manifest": 2, "config": 2, "layer": 2 * len(img.Layers)} {
		require.Equal(t, float64(count), testutil.ToFloat64(metrics.requests.WithLabelValues(kind, "success")))
		require.Equal(t, uint64(count), pullTestHistogramCount(t, metrics.requestDuration, kind, "success"))
	}
}

func TestPullResponseFailures(t *testing.T) {
	img := pullTestImage(t)
	for kind, desc := range map[string]ocispec.Descriptor{
		"manifest": img.Manifest, "config": img.Config, "layer": img.Layers[0],
	} {
		for _, mode := range []string{"status", "redirect", "truncated", "short", "long", "corrupt", "unverified"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				registry := img.handler()

				data := img.manifest
				if kind != "manifest" {
					data = readImageBlob(t, img, desc)
				}

				data = append([]byte(nil), data...)
				status := http.StatusOK

				switch mode {
				case "status":
					status, data = http.StatusServiceUnavailable, []byte("failed")
				case "redirect":
					status, data = http.StatusTemporaryRedirect, []byte("redirect")
				case "short", "truncated":
					data = data[:len(data)-1]
				case "long":
					data = append(data, 0)
				case "corrupt", "unverified":
					data[0] ^= 1
				}

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasSuffix(r.URL.Path, "/"+desc.Digest.String()) {
						registry.ServeHTTP(w, r)
						return
					}

					if mode == "truncated" {
						w.Header().Set("Content-Length", strconv.FormatInt(desc.Size, 10))
					}

					if mode == "redirect" {
						w.Header().Set("Location", "/unexpected-redirect")
					}

					w.WriteHeader(status)
					w.Write(data)
				}))
				t.Cleanup(server.Close)
				opts := pullTestOptions(server.URL)
				opts.LayerConcurrency = 1
				opts.Verify = mode != "unverified"
				p, metrics := pullTestNew(t, img, opts)
				err := p.pull(t.Context())
				expectedBytes := int64(len(data))
				result := "error"

				if mode == "unverified" {
					require.NoError(t, err)

					result = "success"

					expectedBytes = img.Manifest.Size + img.Config.Size
					for _, layer := range img.Layers {
						expectedBytes += layer.Size
					}
				} else {
					require.Error(t, err)

					if kind != "manifest" {
						expectedBytes += img.Manifest.Size
					}

					if kind == "layer" {
						expectedBytes += img.Config.Size
					}
				}

				require.Equal(t, float64(expectedBytes), testutil.ToFloat64(metrics.receivedBytes))
				require.Equal(t, float64(1), testutil.ToFloat64(metrics.pulls.WithLabelValues(result)))
				require.Equal(t, uint64(1), pullTestHistogramCount(t, metrics.pullDuration, result))

				if mode != "unverified" {
					require.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues(kind, "error")))
					require.Equal(t, uint64(1), pullTestHistogramCount(t, metrics.requestDuration, kind, "error"))
					require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
				}

				require.Zero(t, testutil.ToFloat64(metrics.inFlight))
			})
		}
	}
}

func TestPullTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"manifest", "layer"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", kind, canceled), func(t *testing.T) {
				img := pullTestImage(t)
				registry := img.handler()
				started := make(chan struct{}, len(img.Layers))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					blocked := strings.Contains(r.URL.Path, "/manifests/")
					if kind == "layer" {
						blocked = strings.HasSuffix(r.URL.Path, "/"+img.Layers[0].Digest.String())
					}

					if !blocked {
						registry.ServeHTTP(w, r)
						return
					}

					w.WriteHeader(http.StatusOK)
					w.Write([]byte("partial"))
					w.(http.Flusher).Flush()

					started <- struct{}{}

					<-r.Context().Done()
				}))
				t.Cleanup(server.Close)
				opts := pullTestOptions(server.URL)

				opts.LayerConcurrency = 1
				if !canceled {
					opts.Timeout = 150 * time.Millisecond
				}

				p, metrics := pullTestNew(t, img, opts)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)

				done := make(chan error, 1)

				go func() { done <- p.pull(ctx) }()

				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("request did not start")
				}

				expectedBytes := int64(len("partial"))
				if kind == "layer" {
					expectedBytes += img.Manifest.Size + img.Config.Size
				}

				// Ensure partial response bytes were consumed before explicit cancellation.
				require.Eventually(t, func() bool {
					return testutil.ToFloat64(metrics.receivedBytes) == float64(expectedBytes)
				}, time.Second, time.Millisecond)

				result, wantErr := "error", context.DeadlineExceeded
				if canceled {
					result, wantErr = "canceled", context.Canceled

					cancel()
				}

				select {
				case err := <-done:
					require.ErrorIs(t, err, wantErr)
				case <-time.After(3 * time.Second):
					t.Fatal("pull did not finish")
				}

				require.Equal(t, float64(1), testutil.ToFloat64(metrics.pulls.WithLabelValues(result)))
				require.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues(kind, result)))
				require.Zero(t, testutil.ToFloat64(metrics.inFlight))
			})
		}
	}
}

func TestPullLayerFailureCancelsSiblings(t *testing.T) {
	img := pullTestImage(t)
	registry := img.handler()
	siblingStarted := make(chan struct{})
	siblingStopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+img.Layers[0].Digest.String()):
			select {
			case <-siblingStarted:
				w.WriteHeader(http.StatusServiceUnavailable)
			case <-r.Context().Done():
			}
		case strings.HasSuffix(r.URL.Path, "/"+img.Layers[1].Digest.String()):
			close(siblingStarted)
			<-r.Context().Done()
			close(siblingStopped)
		default:
			registry.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	p, metrics := pullTestNew(t, img, pullTestOptions(server.URL))
	err := p.pull(t.Context())
	require.ErrorContains(t, err, "503")
	require.False(t, errors.Is(err, context.Canceled))

	select {
	case <-siblingStopped:
	case <-time.After(time.Second):
		t.Fatal("sibling request was not canceled")
	}

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.pulls.WithLabelValues("error")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues("layer", "error")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues("layer", "canceled")))
}

func TestPullRunConcurrencyAndJoin(t *testing.T) {
	for _, concurrency := range []int{1, 3} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			img := pullTestImage(t)
			registry := img.handler()

			var manifests atomic.Int64

			var layers atomic.Int64

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/manifests/") {
					manifests.Add(1)
				}

				if strings.Contains(r.URL.Path, "/blobs/") && !strings.HasSuffix(r.URL.Path, "/"+img.Config.Digest.String()) {
					layers.Add(1)
					<-r.Context().Done()

					return
				}

				registry.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			opts := pullTestOptions(server.URL)
			opts.Concurrency = concurrency
			p, metrics := pullTestNew(t, img, opts)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			done := make(chan struct{})

			go func() {
				p.run(ctx)
				close(done)
			}()

			expected := int64(concurrency * opts.LayerConcurrency)

			require.Eventually(t, func() bool { return layers.Load() >= expected }, time.Second, time.Millisecond)
			require.Never(t, func() bool { return layers.Load() > expected }, 50*time.Millisecond, time.Millisecond)
			require.Equal(t, int64(concurrency), manifests.Load())
			require.Equal(t, float64(concurrency), testutil.ToFloat64(metrics.inFlight))
			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("run did not join its workers")
			}

			require.Zero(t, testutil.ToFloat64(metrics.inFlight))
			require.Equal(t, float64(concurrency), testutil.ToFloat64(metrics.pulls.WithLabelValues("canceled")))
			require.Equal(t, expected, layers.Load(), "cancellation must not schedule additional layers")
		})
	}
}

func TestPullRunPacing(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", failure), func(t *testing.T) {
			img := pullTestImage(t)
			registry := img.handler()
			requests := make(chan time.Time, 8)

			var closed atomic.Int64

			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/manifests/") {
					select {
					case requests <- time.Now():
					default:
					}
				}

				if failure {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}

				registry.ServeHTTP(w, r)
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed.Add(1)
				}
			}
			server.Start()
			t.Cleanup(server.Close)
			opts := pullTestOptions(server.URL)
			opts.LayerConcurrency = 1
			opts.Interval = opts.RetryDelay
			p, metrics := pullTestNew(t, img, opts)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			done := make(chan struct{})

			go func() {
				p.run(ctx)
				close(done)
			}()

			var first time.Time
			select {
			case first = <-requests:
			case <-time.After(time.Second):
				t.Fatal("first pull did not start")
			}

			select {
			case second := <-requests:
				require.GreaterOrEqual(t, second.Sub(first), opts.RetryDelay)
			case <-time.After(3 * time.Second):
				t.Fatal("next pull did not start after delay")
			}

			result := "success"
			if failure {
				result = "error"
			}

			require.Eventually(t, func() bool {
				return testutil.ToFloat64(metrics.pulls.WithLabelValues(result)) >= 2
			}, time.Second, time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("delay did not respond to cancellation")
			}

			require.Equal(t, float64(2), testutil.ToFloat64(metrics.pulls.WithLabelValues(result)))
			require.Eventually(t, func() bool { return closed.Load() == 1 }, time.Second, time.Millisecond)
		})
	}
}

func TestPullRunZeroConcurrency(t *testing.T) {
	img := pullTestImage(t)

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.Concurrency = 0
	p, metrics := pullTestNew(t, img, opts)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan struct{})

	go func() {
		p.run(ctx)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("origin-only run must block until cancellation")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("origin-only run did not stop")
	}

	require.Zero(t, requests.Load())
	require.Zero(t, testutil.ToFloat64(metrics.inFlight))
	require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
	require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("error")))
	require.Zero(t, testutil.ToFloat64(metrics.pulls.WithLabelValues("canceled")))
}

func TestPullOptions(t *testing.T) {
	img := pullTestImage(t)
	for _, target := range []string{
		"", "localhost:5000", "/relative", "ftp://host", "http:///missing", "http://:80", "http://host:bad",
		"http://user@host", "http://user:password@host", "http://host?query=1", "http://host?",
		"http://host#fragment", "http://host#", "http://host/%zz",
	} {
		t.Run(target, func(t *testing.T) {
			_, err := newPuller(img, pullTestOptions(target), pullTestMetrics())
			require.Error(t, err)
		})
	}

	for _, target := range []string{"http://host", "https://host:443", "http://[::1]:5000", "http://host/base/path/"} {
		t.Run(target, func(t *testing.T) {
			pullTestNew(t, img, pullTestOptions(target))
		})
	}

	for name, mutate := range map[string]func(*pullOptions){
		"negative concurrency":       func(o *pullOptions) { o.Concurrency = -1 },
		"concurrency overflow":       func(o *pullOptions) { o.Concurrency = int(^uint(0) >> 1) },
		"zero layer concurrency":     func(o *pullOptions) { o.LayerConcurrency = 0 },
		"negative layer concurrency": func(o *pullOptions) { o.LayerConcurrency = -1 },
		"zero timeout":               func(o *pullOptions) { o.Timeout = 0 },
		"negative timeout":           func(o *pullOptions) { o.Timeout = -1 },
		"zero retry":                 func(o *pullOptions) { o.RetryDelay = 0 },
		"negative retry":             func(o *pullOptions) { o.RetryDelay = -1 },
		"negative interval":          func(o *pullOptions) { o.Interval = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			opts := pullTestOptions("http://host")
			mutate(&opts)
			_, err := newPuller(img, opts, pullTestMetrics())
			require.Error(t, err)
		})
	}

	_, err := newPuller(nil, pullTestOptions("http://host"), pullTestMetrics())
	require.Error(t, err)
	_, err = newPuller(img, pullTestOptions("http://host"), nil)
	require.Error(t, err)
}
