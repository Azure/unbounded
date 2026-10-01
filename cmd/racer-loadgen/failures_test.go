// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestClassifyPullFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"unknown", errors.New("digest mismatch: arbitrary untrusted text"), "other"},
		{"invalid enum", &pullFailure{err: errors.New("secret"), reason: 255}, "other"},
		{"wrapped canceled", fmt.Errorf("wrapped: %w", context.Canceled), "canceled"},
		{"wrapped deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{"network timeout", &pullFailure{reason: failureTransport, err: &url.Error{Op: "Get", URL: "http://secret", Err: &net.DNSError{IsTimeout: true}}}, "timeout"},
		{"truncated status body", &pullFailure{reason: failureTransport, status: 503, err: io.ErrUnexpectedEOF}, "incomplete"},
		{"EOF before response", &pullFailure{reason: failureTransport, err: io.EOF}, "incomplete"},
		{"reset", &pullFailure{reason: failureTransport, err: syscall.ECONNRESET}, "transport"},
		{"complete status", &pullFailure{reason: failureStatus, err: errors.New("503")}, "http_status"},
		{"too large", &pullFailure{reason: failureSize, err: errors.New("size")}, "size_mismatch"},
		{"verified corruption", &pullFailure{reason: failureDigest, err: errors.New("digest")}, "digest_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, classifyFailure(test.err).String())
		})
	}
}

func TestPullFailureLogsBoundedSanitizedAndConcurrent(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := &puller{metrics: newMetrics(reg)}

	var output bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&output, nil))
	now := time.Now()
	err := &pullFailure{
		err:    errors.New("http://user:password@secret/path?token=secret\nforged"),
		reason: failureTransport, kind: "untrusted\nkind", status: 999999,
	}

	p.reportPullFailure(nil, now, logger)
	p.reportPullFailure(context.Canceled, now, logger)
	require.Empty(t, output.String())

	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() { p.reportPullFailure(err, now, logger) })
	}

	workers.Wait()
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	require.Contains(t, output.String(), `"kind":"unknown","http_status":0`)
	// Integrity gets its own slot even while transport logs are suppressed.
	p.reportPullFailure(&pullFailure{err: err, reason: failureDigest, kind: "layer", status: 200}, now, logger)
	require.Equal(t, 2, strings.Count(output.String(), "\n"))
	require.Contains(t, output.String(), `"reason":"digest_mismatch","kind":"layer","http_status":200`)
	p.reportPullFailure(err, now.Add(failureLogInterval-time.Nanosecond), logger)
	require.Equal(t, 2, strings.Count(output.String(), "\n"))
	p.reportPullFailure(err, now.Add(failureLogInterval), logger)
	require.Equal(t, 3, strings.Count(output.String(), "\n"))
	require.Contains(t, output.String(), `"suppressed":100`)

	for _, secret := range []string{"secret", "password", "forged", "untrusted"} {
		require.NotContains(t, output.String(), secret)
	}

	families := gatherLoadgenMetrics(t, reg)
	family := families["racer_loadgen_pull_failures_total"]
	require.Len(t, family.GetMetric(), 3)

	for reason, count := range map[string]float64{"transport": 102, "digest_mismatch": 1, "canceled": 1} {
		require.Equal(t, count, metricWithLabels(t, family, map[string]string{"reason": reason}).GetCounter().GetValue())
	}
}

func TestLiveWorkersReportPullFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.RetryDelay = time.Hour
	p, metrics := pullTestNew(t, pullTestImage(t), opts)
	ctx, cancel := context.WithCancel(t.Context())
	pool := &liveWorkers{changed: make(chan struct{})}

	t.Cleanup(func() { cancel(); pool.workers.Wait() })
	pool.apply(ctx, p, 1)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.pullFailures.WithLabelValues("http_status")) == 1
	}, time.Second, time.Millisecond)
	cancel()
	pool.workers.Wait()
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.pulls.WithLabelValues("error")))
	require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
}
