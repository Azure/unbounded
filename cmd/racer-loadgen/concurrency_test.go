// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func replaceConcurrency(t *testing.T, path, value string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path+".new", []byte(value), 0o600))
	require.NoError(t, os.Rename(path+".new", path))
}

func TestReadConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrency")
	for _, value := range []string{"0", "1\n", " 256 \n"} {
		replaceConcurrency(t, path, value)
		n, err := readConcurrency(path)
		require.NoError(t, err)
		want, err := strconv.Atoi(strings.TrimSpace(value))
		require.NoError(t, err)
		require.Equal(t, want, n)
	}

	for _, value := range []string{"", " \n", "-1", "257", "1.5", "1 2", "2\n3", "many", "999999999999999999999999", strings.Repeat(" ", 65) + "1"} {
		replaceConcurrency(t, path, value)
		_, err := readConcurrency(path)
		require.Error(t, err, "value %q", value)
	}

	_, err := readConcurrency(path + ".missing")
	require.Error(t, err)
	_, err = readConcurrency(filepath.Dir(path))
	require.Error(t, err)
	require.NoError(t, syscall.Mkfifo(path+".fifo", 0o600))
	_, err = readConcurrency(path + ".fifo")
	require.ErrorContains(t, err, "regular file")

	// Match projected ConfigMap symlinks, including replacing the ..data link.
	root := t.TempDir()
	for _, version := range []string{"1", "2"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, version), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, version, "concurrency"), []byte(version), 0o600))
	}

	require.NoError(t, os.Symlink("1", filepath.Join(root, "..data")))
	require.NoError(t, os.Symlink("..data/concurrency", filepath.Join(root, "concurrency")))
	n, err := readConcurrency(filepath.Join(root, "concurrency"))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, os.Symlink("2", filepath.Join(root, "..data.new")))
	require.NoError(t, os.Rename(filepath.Join(root, "..data.new"), filepath.Join(root, "..data")))
	n, err = readConcurrency(filepath.Join(root, "concurrency"))
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestLiveConcurrencyOptions(t *testing.T) {
	opts, err := parseOptions([]string{"--concurrency-file=/control/concurrency"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "/control/concurrency", opts.pull.ConcurrencyFile)

	for _, n := range []int{0, maxLiveConcurrency, maxLiveConcurrency + 1} {
		opts.pull.Concurrency = n

		p, err := newPuller(&syntheticImage{}, opts.pull, pullTestMetrics())
		if n > maxLiveConcurrency {
			require.Error(t, err)
			continue
		}

		require.NoError(t, err)
		require.Equal(t, maxLiveConcurrency*opts.pull.LayerConcurrency, p.transport.MaxIdleConnsPerHost)
		p.transport.CloseIdleConnections()
	}
	// The optional mode must not impose a new limit on legacy CLI-only runs.
	opts.pull.ConcurrencyFile = ""
	p, err := newPuller(&syntheticImage{}, opts.pull, pullTestMetrics())
	require.NoError(t, err)
	p.transport.CloseIdleConnections()

	opts.pull.ConcurrencyFile = "/control/concurrency"
	opts.pull.Concurrency = 0
	opts.pull.LayerConcurrency = int(^uint(0) >> 1)
	_, err = newPuller(&syntheticImage{}, opts.pull, pullTestMetrics())
	require.ErrorContains(t, err, "too large")
}

func TestLiveConcurrencyUpdatesDrainAndShutdown(t *testing.T) {
	img := pullTestImage(t)
	release := make(chan struct{}, maxLiveConcurrency)

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			requests.Add(1)

			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}

		img.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.ConcurrencyFile = filepath.Join(t.TempDir(), "concurrency")
	p, metrics := pullTestNew(t, img, opts)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() { p.runLive(ctx, 5*time.Millisecond); close(done) }()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("live workers failed to join")
		}
	})

	await := func(n int) {
		t.Helper()
		require.Eventually(t, func() bool {
			return testutil.ToFloat64(metrics.appliedConcurrency) == float64(n)
		}, time.Second, time.Millisecond)
	}
	// Missing initial config falls back to the CLI, rather than silently stopping.
	await(1)
	require.Eventually(t, func() bool { return requests.Load() == 1 }, time.Second, time.Millisecond)
	replaceConcurrency(t, opts.ConcurrencyFile, "3")
	await(3)
	require.Eventually(t, func() bool { return requests.Load() == 3 }, time.Second, time.Millisecond)

	for _, value := range []string{"257", "", "broken"} {
		replaceConcurrency(t, opts.ConcurrencyFile, value)
		require.Never(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) != 3 }, 30*time.Millisecond, time.Millisecond)
	}

	require.NoError(t, os.Remove(opts.ConcurrencyFile))
	require.Never(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) != 3 }, 30*time.Millisecond, time.Millisecond)
	replaceConcurrency(t, opts.ConcurrencyFile, "1")
	await(1)
	require.Equal(t, float64(3), testutil.ToFloat64(metrics.inFlight), "shrink must not cancel admitted pulls")
	replaceConcurrency(t, opts.ConcurrencyFile, "0")
	await(0)

	for range 3 {
		release <- struct{}{}
	}

	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.inFlight) == 0 }, time.Second, time.Millisecond)
	require.Equal(t, float64(3), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
	require.Positive(t, testutil.ToFloat64(metrics.verifiedBytes))
	require.Never(t, func() bool { return requests.Load() != 3 }, 30*time.Millisecond, time.Millisecond)
	replaceConcurrency(t, opts.ConcurrencyFile, "2")
	await(2)
	require.Eventually(t, func() bool { return requests.Load() == 5 }, time.Second, time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel in-flight pulls and join workers")
	}

	require.Zero(t, testutil.ToFloat64(metrics.inFlight))
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.pulls.WithLabelValues("canceled")))
}

func TestLiveWorkerSlotsBoundedAndPacingShutdown(t *testing.T) {
	img := pullTestImage(t)
	server := httptest.NewServer(img.handler())
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.Interval = time.Hour
	p, metrics := pullTestNew(t, img, opts)
	ctx, cancel := context.WithCancel(t.Context())
	pool := &liveWorkers{changed: make(chan struct{})}

	t.Cleanup(func() { cancel(); pool.workers.Wait() })

	for range 100 {
		pool.apply(ctx, p, maxLiveConcurrency)
		pool.apply(ctx, p, 0)
	}

	require.Equal(t, maxLiveConcurrency, pool.started, "oscillation must reuse bounded slots")
	pool.apply(ctx, p, 1)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.pulls.WithLabelValues("success")) > 0
	}, 3*time.Second, time.Millisecond)
	cancel()

	done := make(chan struct{})

	go func() { pool.workers.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parked or pacing workers ignored shutdown")
	}

	require.Zero(t, testutil.ToFloat64(metrics.inFlight))
}

func TestRunLiveConcurrencyKeepsOriginAndCatalog(t *testing.T) {
	opts := loadgenTestOptions(t)
	loadgenTestAddresses(t, &opts)
	opts.catalogImages = 3
	opts.pull.ConcurrencyFile = filepath.Join(t.TempDir(), "concurrency")
	opts.pull.Target = "http://" + opts.listen
	opts.pull.Interval = 5 * time.Millisecond
	replaceConcurrency(t, opts.pull.ConcurrencyFile, "0")
	running := startLoadgenTest(t, opts)
	client := loadgenTestClient(t)
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)
	endpoint := "http://" + opts.listen + "/v2/test/image/manifests/image-000002"
	status, before, err := loadgenTestGet(client, endpoint)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	for _, n := range []int{2, 0} {
		replaceConcurrency(t, opts.pull.ConcurrencyFile, strconv.Itoa(n))
		require.Eventually(t, func() bool {
			_, body, err := loadgenTestGet(client, "http://"+opts.metricsListen+"/metrics")
			return err == nil && strings.Contains(body, "racer_loadgen_applied_concurrency "+strconv.Itoa(n)+"\n")
		}, 3*time.Second, 10*time.Millisecond)
	}

	families := loadgenTestScrape(t, client, opts.metricsListen)
	require.Positive(t, metricWithLabels(t, families["racer_loadgen_received_bytes_total"], nil).GetCounter().GetValue())
	require.Zero(t, metricWithLabels(t, families["racer_loadgen_verified_bytes_total"], nil).GetCounter().GetValue())

	status, after, err := loadgenTestGet(client, endpoint)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, before, after)
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)
	running.cancel()
	running.wait(t)
	assertLoadgenStopped(t, client, opts)
}
