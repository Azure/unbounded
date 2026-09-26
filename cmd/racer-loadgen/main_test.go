// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
)

func TestParseOptionsDefaults(t *testing.T) {
	var output bytes.Buffer

	opts, err := parseOptions(nil, &output)
	require.NoError(t, err)
	require.Empty(t, output.String())
	require.Equal(t, options{
		listen: ":8080", metricsListen: ":9090", startDelay: 10 * time.Second,
		image: imageOptions{
			Repository: "benchmark/image", Layers: 8, LayerBytes: 64 << 20,
			Jitter: 0.2, Seed: "benchmark-v1",
		},
		pull: pullOptions{
			Target: "http://127.0.0.1:5000", Namespace: "loadgen.invalid",
			Concurrency: 64, LayerConcurrency: 4, Timeout: 2 * time.Minute,
			RetryDelay: time.Second, Verify: true,
		},
	}, opts)
}

func TestParseOptionsOverrides(t *testing.T) {
	opts, err := parseOptions([]string{
		"--listen=127.0.0.1:8001", "--metrics-listen=127.0.0.1:9001",
		"--repository=custom/image", "--layers=2", "--layer-bytes=4096", "--jitter=0", "--seed=custom",
		"--target=https://mirror.example/base", "--namespace=registry.example:5000",
		"--concurrency=0", "--layer-concurrency=3", "--pull-timeout=9s",
		"--retry-delay=20ms", "--interval=30ms", "--verify=false", "--start-delay=0", "--duration=1m",
	}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, options{
		listen: "127.0.0.1:8001", metricsListen: "127.0.0.1:9001", duration: time.Minute,
		image: imageOptions{Repository: "custom/image", Layers: 2, LayerBytes: 4096, Seed: "custom"},
		pull: pullOptions{
			Target: "https://mirror.example/base", Namespace: "registry.example:5000",
			Concurrency: 0, LayerConcurrency: 3, Timeout: 9 * time.Second,
			RetryDelay: 20 * time.Millisecond, Interval: 30 * time.Millisecond,
		},
	}, opts)
}

func TestParseOptionsInvalid(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--unknown"}, "flag provided but not defined"},
		{"missing value", []string{"--layers"}, "flag needs an argument"},
		{"bad integer", []string{"--concurrency=many"}, "invalid value"},
		{"integer overflow", []string{"--layer-bytes=9223372036854775808"}, "invalid value"},
		{"bad duration", []string{"--duration=soon"}, "invalid value"},
		{"bad boolean", []string{"--verify=perhaps"}, "invalid boolean value"},
		{"bad float", []string{"--jitter=some"}, "invalid value"},
		{"negative start delay", []string{"--start-delay=-1ns"}, "must be nonnegative"},
		{"negative duration", []string{"--duration=-1ns"}, "must be nonnegative"},
		{"positional", []string{"image"}, "unexpected positional arguments"},
		{"after separator", []string{"--", "image"}, "unexpected positional arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOptions(test.args, io.Discard)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestParseOptionsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var output bytes.Buffer

			_, err := parseOptions([]string{arg}, &output)
			require.ErrorIs(t, err, flag.ErrHelp)

			for _, text := range []string{
				"Usage of racer-loadgen:", "-listen", "-metrics-listen", "-repository",
				"-layers", "-layer-bytes", "-jitter", "-seed", "-target", "-namespace",
				"-concurrency", "zero serves only the origin", "-layer-concurrency",
				"-pull-timeout", "-retry-delay", "-interval", "-verify", "-start-delay", "-duration",
			} {
				require.Contains(t, output.String(), text)
			}
		})
	}
}

func loadgenTestOptions(t *testing.T) options {
	t.Helper()

	opts, err := parseOptions(nil, io.Discard)
	require.NoError(t, err)

	opts.listen = "127.0.0.1:0"
	opts.metricsListen = "127.0.0.1:0"
	opts.image = imageOptions{Repository: "test/image", Layers: 2, LayerBytes: 1024, Seed: "lifecycle"}
	opts.pull.Concurrency = 1
	opts.pull.Interval = time.Hour
	opts.startDelay = 0

	return opts
}

// run owns its listeners and does not expose addresses assigned for port zero.
// Probe both ports together so they are distinct before handing them to run.
func loadgenTestAddresses(t *testing.T, opts *options) {
	t.Helper()

	origin, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = origin.Close() })

	metrics, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = metrics.Close() })

	opts.listen = origin.Addr().String()
	opts.metricsListen = metrics.Addr().String()

	require.NoError(t, origin.Close())
	require.NoError(t, metrics.Close())
}

type loadgenTestRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startLoadgenTest(t *testing.T, opts options) *loadgenTestRun {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	running := &loadgenTestRun{cancel: cancel, done: make(chan struct{})}

	go func() {
		running.err = run(ctx, opts)
		close(running.done)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-running.done:
		case <-time.After(10 * time.Second):
			t.Error("run did not stop after cancellation")
		}
	})

	return running
}

func (running *loadgenTestRun) wait(t *testing.T) {
	t.Helper()

	select {
	case <-running.done:
		require.NoError(t, running.err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
}

func loadgenTestClient(t *testing.T) *http.Client {
	t.Helper()

	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport, Timeout: time.Second}
}

func loadgenTestGet(client *http.Client, endpoint string) (int, string, error) {
	response, err := client.Get(endpoint)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)

	return response.StatusCode, string(body), err
}

func awaitLoadgenStatus(t *testing.T, client *http.Client, endpoint string, want int) {
	t.Helper()

	require.Eventually(t, func() bool {
		status, _, err := loadgenTestGet(client, endpoint)

		return err == nil && status == want
	}, 5*time.Second, 5*time.Millisecond, "waiting for %s to return %d", endpoint, want)
}

func assertLoadgenStopped(t *testing.T, client *http.Client, opts options) {
	t.Helper()

	for _, addr := range []string{opts.listen, opts.metricsListen} {
		_, _, err := loadgenTestGet(client, "http://"+addr+"/healthz")
		require.Error(t, err, "listener %s still accepts requests", addr)
	}
}

func loadgenTestScrape(t *testing.T, client *http.Client, addr string) map[string]*dto.MetricFamily {
	t.Helper()

	status, body, err := loadgenTestGet(client, "http://"+addr+"/metrics")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	require.NoError(t, err)

	return families
}

func TestRunBaselineSelfPull(t *testing.T) {
	for _, stop := range []string{"duration", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			opts := loadgenTestOptions(t)
			loadgenTestAddresses(t, &opts)

			opts.pull.Target = "http://" + opts.listen
			if stop == "duration" {
				opts.duration = 2 * time.Second
			}

			img, err := newImage(t.Context(), opts.image)
			require.NoError(t, err)

			wantBytes := img.Manifest.Size + img.Config.Size
			for _, layer := range img.Layers {
				wantBytes += layer.Size
			}

			client := loadgenTestClient(t)
			started := time.Now()
			running := startLoadgenTest(t, opts)
			awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)
			awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/healthz", http.StatusOK)
			require.Eventually(t, func() bool {
				_, body, err := loadgenTestGet(client, "http://"+opts.metricsListen+"/metrics")

				return err == nil && strings.Contains(body, `racer_loadgen_pulls_total{result="success"} 1`)
			}, time.Second, 5*time.Millisecond)

			families := loadgenTestScrape(t, client, opts.metricsListen)
			for _, name := range []string{"received_bytes_total", "origin_bytes_total"} {
				require.Equal(t, float64(wantBytes), metricWithLabels(t, families["racer_loadgen_"+name], nil).GetCounter().GetValue())
			}

			require.Zero(t, metricWithLabels(t, families["racer_loadgen_in_flight"], nil).GetGauge().GetValue())
			require.Equal(t, float64(1), metricWithLabels(t, families["racer_loadgen_pulls_total"], map[string]string{"result": "success"}).GetCounter().GetValue())
			require.Len(t, families["racer_loadgen_pulls_total"].GetMetric(), 1, "baseline must not fail or cancel a pull")
			require.Equal(t, uint64(1), metricWithLabels(t, families["racer_loadgen_pull_duration_seconds"], map[string]string{"result": "success"}).GetHistogram().GetSampleCount())

			for kind, count := range map[string]float64{"manifest": 1, "config": 1, "layer": 2} {
				labels := map[string]string{"kind": kind, "result": "success"}
				require.Equal(t, count, metricWithLabels(t, families["racer_loadgen_requests_total"], labels).GetCounter().GetValue())
				require.Equal(t, uint64(count), metricWithLabels(t, families["racer_loadgen_request_duration_seconds"], labels).GetHistogram().GetSampleCount())
			}

			require.Contains(t, families, "go_goroutines")
			require.Contains(t, families, "process_cpu_seconds_total")

			if stop == "cancel" {
				running.cancel()
			}

			running.wait(t)

			if stop == "duration" {
				require.GreaterOrEqual(t, time.Since(started), opts.duration, "duration must not stop workers early")
			}

			assertLoadgenStopped(t, client, opts)
		})
	}
}

func TestRunInitializingHealthAndReadiness(t *testing.T) {
	opts := loadgenTestOptions(t)
	loadgenTestAddresses(t, &opts)
	// Virtual layers use bounded memory. Hashing this layer cannot finish before
	// the probes, and cancellation must interrupt hashing rather than wait for it.
	opts.image.LayerBytes = 1 << 40
	client := loadgenTestClient(t)
	running := startLoadgenTest(t, opts)
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/healthz", http.StatusOK)
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusServiceUnavailable)
	awaitLoadgenStatus(t, client, "http://"+opts.listen+"/v2/", http.StatusServiceUnavailable)
	families := loadgenTestScrape(t, client, opts.metricsListen)
	require.Zero(t, metricWithLabels(t, families["racer_loadgen_received_bytes_total"], nil).GetCounter().GetValue())
	require.NotContains(t, families, "racer_loadgen_pulls_total")
	running.cancel()
	running.wait(t)
	assertLoadgenStopped(t, client, opts)
}

func TestRunOriginOnlyAndStartDelay(t *testing.T) {
	for _, mode := range []string{"origin only", "start delay"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int64

			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(target.Close)
			opts := loadgenTestOptions(t)
			loadgenTestAddresses(t, &opts)

			opts.pull.Target = target.URL
			if mode == "origin only" {
				opts.pull.Concurrency = 0
			} else {
				opts.startDelay = time.Hour
			}

			client := loadgenTestClient(t)
			running := startLoadgenTest(t, opts)
			awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)
			awaitLoadgenStatus(t, client, "http://"+opts.listen+"/v2/test/image/manifests/latest", http.StatusOK)

			select {
			case <-running.done:
				t.Fatalf("run exited before cancellation: %v", running.err)
			case <-time.After(25 * time.Millisecond):
			}

			families := loadgenTestScrape(t, client, opts.metricsListen)
			require.NotContains(t, families, "racer_loadgen_pulls_total")
			require.Zero(t, metricWithLabels(t, families["racer_loadgen_in_flight"], nil).GetGauge().GetValue())
			running.cancel()
			running.wait(t)
			require.Zero(t, requests.Load(), "workers must not contact the pull target")
			assertLoadgenStopped(t, client, opts)
		})
	}
}

func TestRunBindFailure(t *testing.T) {
	for _, address := range []string{"origin", "metrics"} {
		t.Run(address, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })

			opts := loadgenTestOptions(t)
			if address == "origin" {
				opts.listen = listener.Addr().String()
			} else {
				// The origin uses port zero; its server must also stop when the
				// second bind fails, without waiting for worker cancellation.
				opts.metricsListen = listener.Addr().String()
			}

			running := startLoadgenTest(t, opts)
			select {
			case <-running.done:
				require.ErrorContains(t, running.err, "listen "+listener.Addr().String())

				var opErr *net.OpError
				require.ErrorAs(t, running.err, &opErr)
				require.Equal(t, "listen", opErr.Op)
			case <-time.After(5 * time.Second):
				t.Fatal("bind failure did not stop run")
			}
		})
	}
}

func TestRunInvalidOptions(t *testing.T) {
	for _, arg := range []string{
		"--layers=0", "--layer-bytes=0", "--jitter=1", "--jitter=NaN", "--repository=UPPER/image",
		"--concurrency=-1", "--layer-concurrency=0", "--pull-timeout=0", "--retry-delay=0",
		"--interval=-1s", "--target=ftp://example.com", "--target=http://user:pass@example.com",
		"--target=http://example.com?query=value", "--target=http://example.com#fragment",
	} {
		t.Run(arg, func(t *testing.T) {
			opts, err := parseOptions([]string{
				"--listen=127.0.0.1:0", "--metrics-listen=127.0.0.1:0", "--layers=1", "--layer-bytes=1", arg,
			}, io.Discard)
			require.NoError(t, err)

			running := startLoadgenTest(t, opts)
			select {
			case <-running.done:
				require.Error(t, running.err)
			case <-time.After(5 * time.Second):
				t.Fatal("invalid options were not rejected")
			}
		})
	}
}
