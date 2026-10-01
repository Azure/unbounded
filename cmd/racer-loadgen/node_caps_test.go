// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestNodeCapStrictParsing(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"version":1,"caps":{"node-a":1}}`, 1},
		{`{"caps":{"node-b":0},"version":1}`, 256},
		{`{"version":1,"caps":{"node-a":0}}`, 0},
	} {
		n, err := parseNodeCap([]byte(tc.body), "node-a")
		require.NoError(t, err)
		require.Equal(t, tc.want, n)
	}

	for _, body := range []string{
		`null`, `{}`, `{"version":2,"caps":{}}`, `{"version":1,"caps":{},"extra":0}`,
		`{"version":1,"version":1,"caps":{}}`, `{"version":1,"caps":{"node-a":1,"node-a":2}}`,
		`{"version":1,"caps":null}`, `{"version":1,"caps":{}} {}`,
		strings.Repeat(" ", maxNodeCapsBytes+1),
	} {
		_, err := parseNodeCap([]byte(body), "node-a")
		require.Error(t, err, body)
	}

	for _, value := range []string{"null", "-1", "257", "1.5", "1e0", `"1"`, "true"} {
		_, err := parseNodeCap([]byte(`{"version":1,"caps":{"node-a":`+value+`}}`), "node-a")
		require.Error(t, err, value)
	}

	for _, name := range []string{"", "Node-A", "node/a", "-node", strings.Repeat("a", 254)} {
		_, err := parseNodeCap(fmt.Appendf(nil, `{"version":1,"caps":{%q:1}}`, name), "node-a")
		require.Error(t, err)
	}

	entries := make([]string, 257)
	for i := range entries {
		entries[i] = fmt.Sprintf(`"node-%d":1`, i)
	}

	_, err := parseNodeCap([]byte(`{"version":1,"caps":{`+strings.Join(entries, ",")+`}}`), "node-a")
	require.Error(t, err)
}

func TestNodeCapsDrain(t *testing.T) {
	img := pullTestImage(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		img.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	root := t.TempDir()
	opts.ConcurrencyFile = filepath.Join(root, "concurrency")
	opts.NodeCapsFile = filepath.Join(root, "caps")
	opts.NodeName = "node-a"

	capProjection(t, root, "start", "8", `{"version":1,"caps":{"node-a":2}}`)
	p, metrics := pullTestNew(t, img, opts)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() { p.runLive(ctx, 5*time.Millisecond); close(done) }()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker shutdown timeout")
		}
	})
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.inFlight) == 2 }, time.Second, time.Millisecond)
	capProjection(t, root, "shrink", "8", `{"version":1,"caps":{"node-a":1}}`)
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) == 1 }, time.Second, time.Millisecond)
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.inFlight))
	capProjection(t, root, "pause", "0", "broken")
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) == 0 }, time.Second, time.Millisecond)
	close(release)
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.inFlight) == 0 }, time.Second, time.Millisecond)
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.pulls.WithLabelValues("success")))
	require.Positive(t, testutil.ToFloat64(metrics.verifiedBytes))
	capProjection(t, root, "bad-resume", "8", "missing")
	require.Never(t, func() bool { return testutil.ToFloat64(metrics.appliedConcurrency) != 0 }, 30*time.Millisecond, time.Millisecond)
}

func TestNodeCapsKeepOriginAndCatalog(t *testing.T) {
	opts := loadgenTestOptions(t)
	loadgenTestAddresses(t, &opts)
	opts.catalogImages = 3
	root := t.TempDir()
	opts.pull.ConcurrencyFile = filepath.Join(root, "concurrency")
	opts.pull.NodeCapsFile = filepath.Join(root, "caps")
	opts.pull.NodeName = "node-a"
	opts.pull.Target = "http://" + opts.listen
	opts.pull.Interval = 5 * time.Millisecond

	capProjection(t, root, "start", "8", `{"version":1,"caps":{"node-a":0}}`)
	running := startLoadgenTest(t, opts)
	client := loadgenTestClient(t)
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)
	endpoint := "http://" + opts.listen + "/v2/test/image/manifests/image-000002"
	status, before, err := loadgenTestGet(client, endpoint)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	for i, n := range []int{1, 0} {
		capProjection(t, root, fmt.Sprint(i), "8", fmt.Sprintf(`{"version":1,"caps":{"node-a":%d}}`, n))
		require.Eventually(t, func() bool {
			_, body, err := loadgenTestGet(client, "http://"+opts.metricsListen+"/metrics")
			return err == nil && strings.Contains(body, fmt.Sprintf("racer_loadgen_applied_concurrency %d\n", n))
		}, 3*time.Second, 10*time.Millisecond)
	}

	status, after, err := loadgenTestGet(client, endpoint)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, before, after)
	families := loadgenTestScrape(t, client, opts.metricsListen)
	require.Positive(t, metricWithLabels(t, families["racer_loadgen_verified_bytes_total"], nil).GetCounter().GetValue())
	awaitLoadgenStatus(t, client, "http://"+opts.metricsListen+"/readyz", http.StatusOK)
	running.cancel()
	running.wait(t)
	assertLoadgenStopped(t, client, opts)
}

func capProjection(t *testing.T, root, version, global, caps string) {
	t.Helper()

	dir := filepath.Join(root, version)
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "concurrency"), []byte(global), 0o600))

	if caps != "missing" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "caps"), []byte(caps), 0o600))
	}

	require.NoError(t, os.Symlink(version, filepath.Join(root, "..next")))
	require.NoError(t, os.Rename(filepath.Join(root, "..next"), filepath.Join(root, "..data")))
}

func TestNodeCapProjectionAndFallback(t *testing.T) {
	root := t.TempDir()
	opts := pullOptions{ConcurrencyFile: filepath.Join(root, "concurrency"), NodeCapsFile: filepath.Join(root, "caps"), NodeName: "node-a"}

	s := nodeCapState{global: 8, cap: 256}
	for i, tc := range []struct {
		global, caps string
		want         int
		bad          bool
	}{
		{"8", "missing", 0, true},
		{"8", `{"version":1,"caps":{"node-a":1}}`, 1, false},
		{"12", "broken", 1, true},
		{"0", "broken", 0, true},
		{"8", "missing", 0, true},
		{"8", `{"version":1,"caps":{"node-a":1}}`, 1, false},
		{"bad", `{"version":1,"caps":{}}`, 1, true},
		{"8", `{"version":1,"caps":{}}`, 8, false},
		{"2", "broken", 2, true},
	} {
		capProjection(t, root, fmt.Sprint(i), tc.global, tc.caps)

		n, err := s.poll(opts)
		require.Equal(t, tc.bad, err != nil)
		require.Equal(t, tc.want, n)
	}
	// Visible keys deliberately disagree. The reader must use the pinned generation.
	require.NoError(t, os.WriteFile(opts.ConcurrencyFile, []byte("256"), 0o600))
	require.NoError(t, os.WriteFile(opts.NodeCapsFile, []byte(`{"version":1,"caps":{}}`), 0o600))
	capProjection(t, root, "coherent", "8", `{"version":1,"caps":{"node-a":1}}`)

	n, err := s.poll(opts)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, os.Remove(filepath.Join(root, "..data")))
	replaceConcurrency(t, opts.ConcurrencyFile, "0")
	n, err = s.poll(opts)
	require.Error(t, err)
	require.Zero(t, n)
}

func TestNodeCapOptions(t *testing.T) {
	parsed, err := parseOptions([]string{"--concurrency-file=/control/concurrency", "--node-concurrency-caps-file=/control/caps", "--node-name=node-a"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "/control/caps", parsed.pull.NodeCapsFile)
	require.Equal(t, "node-a", parsed.pull.NodeName)
	require.NoError(t, validateNodeCapsOptions(parsed.pull))
	parsed.pull.ConcurrencyFile = ""
	_, err = newPuller(pullTestImage(t), parsed.pull, pullTestMetrics())
	require.Error(t, err)

	opts := pullOptions{ConcurrencyFile: "/control/concurrency", NodeCapsFile: "/control/caps", NodeName: "node-a"}
	require.NoError(t, validateNodeCapsOptions(opts))
	opts.NodeName = ""
	require.Error(t, validateNodeCapsOptions(opts))
	opts.NodeName = "node-a"
	opts.NodeCapsFile = "/other/caps"
	require.Error(t, validateNodeCapsOptions(opts))
	opts.NodeCapsFile = opts.ConcurrencyFile
	require.Error(t, validateNodeCapsOptions(opts))
	opts.NodeCapsFile = ""
	require.NoError(t, validateNodeCapsOptions(opts))
}

func TestNodeCapFileRejectionHonorsPause(t *testing.T) {
	for _, kind := range []string{"fifo", "directory", "oversize", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			capProjection(t, root, "generation", "0", "missing")
			path := filepath.Join(root, "generation", "caps")

			switch kind {
			case "fifo":
				require.NoError(t, syscall.Mkfifo(path, 0o600))
			case "directory":
				require.NoError(t, os.Mkdir(path, 0o700))
			case "oversize":
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat(" ", maxNodeCapsBytes+1)), 0o600))
			}

			s := nodeCapState{global: 8, cap: 1, effective: 1}
			n, err := s.poll(pullOptions{ConcurrencyFile: filepath.Join(root, "concurrency"), NodeCapsFile: filepath.Join(root, "caps"), NodeName: "node-a"})
			require.Error(t, err)
			require.Zero(t, n)
		})
	}
}

func TestNodeCapsConcurrentProjectionSwap(t *testing.T) {
	root := t.TempDir()
	capProjection(t, root, "paused", "0", `{"version":1,"caps":{"node-a":256}}`)
	capProjection(t, root, "capped", "8", `{"version":1,"caps":{"node-a":0}}`)

	for _, name := range []string{"concurrency", "caps"} {
		require.NoError(t, os.Symlink(filepath.Join("..data", name), filepath.Join(root, name)))
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		for ctx.Err() == nil {
			for _, generation := range []string{"paused", "capped"} {
				if err := os.Symlink(generation, filepath.Join(root, "..next")); err != nil {
					done <- err
					return
				}

				if err := os.Rename(filepath.Join(root, "..next"), filepath.Join(root, "..data")); err != nil {
					done <- err
					return
				}
			}
		}

		done <- nil
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Error("projection writer failed to join")
		}
	})

	s := nodeCapState{cap: 256}

	opts := pullOptions{ConcurrencyFile: filepath.Join(root, "concurrency"), NodeCapsFile: filepath.Join(root, "caps"), NodeName: "node-a"}
	for range 200 {
		n, err := s.poll(opts)
		require.NoError(t, err)
		require.Zero(t, n, "mixing capped global with paused cap could incorrectly admit eight pulls")
	}
}
