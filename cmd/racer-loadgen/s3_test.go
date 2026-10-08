// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestS3Options(t *testing.T) {
	opts, err := parseOptions([]string{"--backend=s3"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, s3Options{Endpoint: "http://127.0.0.1:8080", Bucket: "benchmark", Count: 128, Bytes: 64 << 20}, opts.s3)
	require.Equal(t, opts.s3.Endpoint, opts.pull.Target)
	require.Equal(t, 1, opts.pull.LayerConcurrency)
	opts, err = parseOptions([]string{"--backend=s3", "--endpoint=https://origin:443/", "--bucket=my-bucket", "--object-count=3", "--object-bytes=123", "--s3-origin", "--concurrency=0", "--profile=zipf"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, s3Options{Endpoint: "https://origin:443/", Bucket: "my-bucket", Count: 3, Bytes: 123, Origin: true}, opts.s3)

	for _, flag := range []string{"--endpoint=http://origin", "--bucket=test", "--object-count=1", "--object-bytes=1", "--s3-origin=false"} {
		_, err := parseOptions([]string{flag}, io.Discard)
		require.Error(t, err)
	}

	for _, flag := range []string{
		"--endpoint=", "--endpoint=ftp://origin", "--endpoint=http://origin/base", "--endpoint=http://origin?x=1", "--endpoint=http://origin?",
		"--endpoint=http://user:pass@origin", "--endpoint=http://origin#", "--endpoint=http://origin:", "--endpoint=http://origin:65536",
		"--bucket=", "--bucket=bad/bucket", "--object-count=0", "--object-count=513", "--object-bytes=0", "--object-bytes=-1",
		"--target=http://origin", "--namespace=store", "--cache=test", "--volume=test", "--repository=test", "--catalog-blobs=1", "--blob-bytes=1",
		"--catalog-images=1", "--layers=1", "--layer-bytes=1", "--jitter=0", "--blob-concurrency=2", "--layer-concurrency=2",
	} {
		t.Run(flag, func(t *testing.T) {
			_, err := parseOptions([]string{"--backend=s3", flag}, io.Discard)
			require.Error(t, err)
		})
	}
}

func s3TestCatalog(t *testing.T, count int, size int64) *blobCatalog {
	t.Helper()
	c, err := newBlobCatalog(t.Context(), "benchmark/s3", "s3-test", count, size)
	require.NoError(t, err)

	return c
}

func s3TestPuller(t *testing.T, c *blobCatalog, endpoint string) (*puller, *loadMetrics) {
	t.Helper()

	opts := pullTestOptions(endpoint)
	opts.Backend = "s3"
	opts.LayerConcurrency = 1
	p, m := pullTestNew(t, &syntheticImage{}, opts)
	p.batches = c.batches
	p.configureS3(c, "benchmark")

	return p, m
}

func TestS3OriginHTTP(t *testing.T) {
	c := s3TestCatalog(t, 2, 123)
	desc := c.batches[0].blobs[0].descriptor
	etag := `"` + desc.Digest.String() + `"`
	data, err := io.ReadAll(io.NewSectionReader(c.blobs[desc.Digest].data, 0, desc.Size))
	require.NoError(t, err)

	for _, test := range []struct {
		name, method, path string
		headers            map[string]string
		status             int
		first, length      int
		code               string
	}{
		{name: "get", method: "GET", status: 200, length: 123},
		{name: "head", method: "HEAD", status: 200, length: 123},
		{name: "range", method: "GET", headers: map[string]string{"Range": "bytes=3-17"}, status: 206, first: 3, length: 15},
		{name: "open", method: "GET", headers: map[string]string{"Range": "bytes=120-"}, status: 206, first: 120, length: 3},
		{name: "suffix", method: "GET", headers: map[string]string{"Range": "bytes=-3"}, status: 206, first: 120, length: 3},
		{name: "clamped", method: "GET", headers: map[string]string{"Range": "bytes=120-999"}, status: 206, first: 120, length: 3},
		{name: "head range", method: "HEAD", headers: map[string]string{"Range": "bytes=1-2"}, status: 206, first: 1, length: 2},
		{name: "match", method: "GET", headers: map[string]string{"If-Match": `"other", ` + etag}, status: 200, length: 123},
		{name: "wildcard", method: "GET", headers: map[string]string{"If-Match": "*"}, status: 200, length: 123},
		{name: "mismatch", method: "GET", headers: map[string]string{"If-Match": `"old"`}, status: 412, code: "PreconditionFailed"},
		{name: "weak match", method: "HEAD", headers: map[string]string{"If-Match": "W/" + etag}, status: 412},
		{name: "not modified", method: "GET", headers: map[string]string{"If-None-Match": etag}, status: 304},
		{name: "weak not modified", method: "GET", headers: map[string]string{"If-None-Match": "W/" + etag}, status: 304},
		{name: "wildcard not modified", method: "HEAD", headers: map[string]string{"If-None-Match": "*"}, status: 304},
		{name: "sdk query", method: "GET", path: "/benchmark/object-000000?x-id=GetObject", status: 200, length: 123},
		{name: "unknown", method: "GET", path: "/benchmark/object-000002", status: 404, code: "NoSuchKey"},
		{name: "noncanonical", method: "GET", path: "/benchmark/object-0", status: 404, code: "NoSuchKey"},
		{name: "bucket", method: "GET", path: "/other/object-000000", status: 404, code: "NoSuchBucket"},
		{name: "version", method: "GET", path: "/benchmark/object-000000?versionId=v1", status: 404, code: "NoSuchVersion"},
		{name: "query", method: "GET", path: "/benchmark/object-000000?acl", status: 501, code: "NotImplemented"},
		{name: "malformed query", method: "GET", path: "/benchmark/object-000000?x=%zz", status: 400, code: "InvalidArgument"},
		{name: "write", method: "PUT", status: 405, code: "MethodNotAllowed"},
		{name: "date", method: "GET", headers: map[string]string{"If-Modified-Since": "Tue, 06 Oct 2026 00:00:00 GMT"}, status: 501, code: "NotImplemented"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := test.path
			if path == "" {
				path = "/benchmark/object-000000"
			}

			req := httptest.NewRequest(test.method, path, nil)
			for k, v := range test.headers {
				req.Header.Set(k, v)
			}

			w := httptest.NewRecorder()
			c.s3Handler("benchmark").ServeHTTP(w, req)
			require.Equal(t, test.status, w.Code)

			if test.method == "HEAD" || test.status == 304 {
				require.Empty(t, w.Body.Bytes())
			}

			if test.status == 200 || test.status == 206 {
				require.Equal(t, etag, w.Header().Get("ETag"))
				require.Equal(t, fmt.Sprint(test.length), w.Header().Get("Content-Length"))

				if test.method == "GET" {
					require.Equal(t, data[test.first:test.first+test.length], w.Body.Bytes())
				}

				if test.status == 206 {
					require.Equal(t, fmt.Sprintf("bytes %d-%d/123", test.first, test.first+test.length-1), w.Header().Get("Content-Range"))
				}
			}

			if test.code != "" {
				require.Contains(t, w.Body.String(), "<Code>"+test.code+"</Code>")
			}
		})
	}

	for _, value := range []string{"bytes=123-", "bytes=4-3", "bytes=-0", "bytes=", "bytes=0-1,3-4", "bytes=+1-2", "items=0-1", "bytes=0-9223372036854775808"} {
		req := httptest.NewRequest("GET", "/benchmark/object-000000", nil)
		req.Header.Set("Range", value)

		w := httptest.NewRecorder()
		c.s3Handler("benchmark").ServeHTTP(w, req)
		require.Equal(t, 416, w.Code, value)
		require.Equal(t, "bytes */123", w.Header().Get("Content-Range"))
		require.Contains(t, w.Body.String(), "<Code>InvalidRange</Code>")
	}
}

func TestS3ClientAgainstOrigin(t *testing.T) {
	// The puller uses the standard S3 client, so the synthetic origin must
	// accept its path-style GetObject requests without any adapter.
	c := s3TestCatalog(t, 2, 64<<10+73)
	originMetrics := newMetrics(prometheus.NewRegistry())

	var userAgent atomic.Value

	origin := httptest.NewServer(originMetrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent.Store(r.Header.Get("User-Agent"))
		require.Equal(t, "GetObject", r.URL.Query().Get("x-id"))
		require.Empty(t, r.Header.Get("Authorization"), "anonymous client must not sign requests")
		c.s3Handler("benchmark").ServeHTTP(w, r)
	})))
	t.Cleanup(origin.Close)

	for _, endpoint := range []string{origin.URL, origin.URL + "/"} {
		p, m := s3TestPuller(t, c, endpoint)
		p.opts.DiagnoseIntegrity = true
		require.NoError(t, p.configureDiagnostics(c))

		for _, batch := range c.batches {
			require.NoError(t, p.pullBatch(t.Context(), batch))
		}

		require.Equal(t, float64(2*(64<<10+73)), testutil.ToFloat64(m.verifiedBytes))
		require.Equal(t, testutil.ToFloat64(m.verifiedBytes), testutil.ToFloat64(m.receivedBytes))
		require.Equal(t, float64(2), testutil.ToFloat64(m.pulls.WithLabelValues("success")))
	}

	require.Contains(t, userAgent.Load(), "aws-sdk-go-v2")
	require.Equal(t, float64(4), testutil.ToFloat64(originMetrics.originRequests.WithLabelValues("GET", "200")))
}

func TestS3ValidBucket(t *testing.T) {
	for _, bucket := range []string{"abc", "benchmark", "my-bucket.v2", "0bucket9", strings.Repeat("a", 63)} {
		require.True(t, validS3Bucket(bucket), bucket)
	}

	for _, bucket := range []string{"", "ab", strings.Repeat("a", 64), "Bucket", "bad/bucket", "-bucket", "bucket-", ".bucket", "bucket.", "a..b", "a_b", "a b"} {
		require.False(t, validS3Bucket(bucket), bucket)
	}
}

func TestS3ClientFailuresAndVerification(t *testing.T) {
	c := s3TestCatalog(t, 1, 123)
	for _, test := range []struct {
		name     string
		status   int
		body     string
		verify   bool
		reason   failureReason
		received int
	}{
		{"corrupt", 200, strings.Repeat("x", 123), true, failureDigest, 123},
		{"short", 200, "short", true, failureIncomplete, 5},
		{"oversized", 200, strings.Repeat("x", 124), true, failureSize, 124},
		// The S3 client consumes error bodies to decode them; no bytes reach the reader.
		{"status", 503, "error", true, failureStatus, 0},
		{"not found", 404, "<Error><Code>NoSuchKey</Code></Error>", true, failureStatus, 0},
		{"redirect", 302, "redirect", true, failureStatus, 0},
		{"partial", 206, "partial", true, failureStatus, 7},
		{"unchecked", 200, strings.Repeat("x", 123), false, failureOther, 123},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, "/benchmark/object-000000", r.URL.Path)
				require.Equal(t, "x-id=GetObject", r.URL.RawQuery, "S3 GetObject without OCI namespace query")
				w.Header().Set("Location", "/should-not-follow")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			p, m := s3TestPuller(t, c, server.URL)
			p.opts.Verify = test.verify

			err := p.pullBatch(t.Context(), c.batches[0])
			if test.verify {
				require.Error(t, err)
				require.Equal(t, test.reason, classifyFailure(err))

				var failure *pullFailure
				require.ErrorAs(t, err, &failure)

				if test.reason == failureStatus {
					require.Equal(t, test.status, failure.status)
				}

				require.Equal(t, float64(1), testutil.ToFloat64(m.pullFailures.WithLabelValues(test.reason.String())))
			} else {
				require.NoError(t, err)
			}

			require.Zero(t, testutil.ToFloat64(m.verifiedBytes))
			require.Equal(t, float64(test.received), testutil.ToFloat64(m.receivedBytes))
			require.Equal(t, int32(1), requests.Load(), "S3 client must not retry or follow redirects")
		})
	}
}

func TestS3ConcurrencyAndCancellation(t *testing.T) {
	c := s3TestCatalog(t, 3, 123)

	var active, peak atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)

		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}

		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	p, m := s3TestPuller(t, c, server.URL)
	p.opts.Concurrency = 3

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() { p.run(ctx); close(done) }()

	require.Eventually(t, func() bool { return peak.Load() == 3 }, time.Second, time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("S3 workers failed to cancel")
	}

	require.Equal(t, float64(3), testutil.ToFloat64(m.pulls.WithLabelValues("canceled")))
	require.Zero(t, testutil.ToFloat64(m.appliedConcurrency))
	require.Zero(t, testutil.ToFloat64(m.inFlight))
	// An independent operation deadline must also interrupt the HTTP body.
	p.opts.Timeout = 20 * time.Millisecond
	require.ErrorIs(t, p.pullBatch(t.Context(), c.batches[0]), context.DeadlineExceeded)
}

func TestS3RunSeparateOriginAndClient(t *testing.T) {
	origin, err := parseOptions([]string{"--backend=s3", "--s3-origin", "--concurrency=0", "--object-count=3", "--object-bytes=123", "--start-delay=0"}, io.Discard)
	require.NoError(t, err)
	loadgenTestAddresses(t, &origin)
	originRun := startLoadgenTest(t, origin)
	client := loadgenTestClient(t)
	awaitLoadgenStatus(t, client, "http://"+origin.metricsListen+"/readyz", 200)
	consumer, err := parseOptions([]string{"--backend=s3", "--endpoint=http://" + origin.listen, "--concurrency=2", "--object-count=3", "--object-bytes=123", "--start-delay=0", "--interval=1h", "--profile=zipf"}, io.Discard)
	require.NoError(t, err)
	loadgenTestAddresses(t, &consumer)
	consumerRun := startLoadgenTest(t, consumer)
	awaitLoadgenStatus(t, client, "http://"+consumer.metricsListen+"/readyz", 200)
	require.Eventually(t, func() bool {
		_, body, err := loadgenTestGet(client, "http://"+consumer.metricsListen+"/metrics")
		return err == nil && strings.Contains(body, `racer_loadgen_pulls_total{result="success"} 2`)
	}, time.Second, time.Millisecond)

	_, _, err = loadgenTestGet(client, "http://"+consumer.listen+"/benchmark/object-000000")
	require.Error(t, err, "S3 client must not bind origin port (sidecar shares the pod)")
	families := loadgenTestScrape(t, client, consumer.metricsListen)
	require.Equal(t, float64(246), metricWithLabels(t, families["racer_loadgen_received_bytes_total"], nil).GetCounter().GetValue())
	require.Zero(t, metricWithLabels(t, families["racer_loadgen_verified_bytes_total"], nil).GetCounter().GetValue())
	originRun.cancel()
	consumerRun.cancel()
	originRun.wait(t)
	consumerRun.wait(t)
	assertLoadgenStopped(t, client, origin)
	assertLoadgenStopped(t, client, consumer)
}
