// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestFailureDiagnostic(t *testing.T) {
	secret := "https://user:password@private.invalid/path?token=secret\nforged"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, ""},
		{"refused", syscall.ECONNREFUSED, "connection refused"},
		{"reset", syscall.ECONNRESET, "connection reset by peer"},
		{"broken pipe", syscall.EPIPE, "broken pipe"},
		{"socket missing", &os.PathError{Op: secret, Path: secret, Err: syscall.ENOENT}, "no such file or directory"},
		{"DNS missing", &net.DNSError{Name: secret, Server: secret, Err: secret, IsNotFound: true}, "DNS lookup: no such host"},
		{"DNS timeout", &net.DNSError{Name: secret, Err: secret, IsTimeout: true}, "DNS lookup: timeout"},
		{"DNS temporary", &net.DNSError{Name: secret, Err: secret, IsTemporary: true}, "DNS lookup: temporary failure"},
		{"DNS other", &net.DNSError{Name: secret, Err: secret}, "DNS lookup failed"},
		{"TLS trust", x509.UnknownAuthorityError{Cert: &x509.Certificate{DNSNames: []string{secret}}}, "TLS: certificate signed by unknown authority"},
		{"TLS hostname", x509.HostnameError{Host: secret}, "TLS: certificate hostname mismatch"},
		{"TLS expired", x509.CertificateInvalidError{Reason: x509.Expired, Detail: secret}, "TLS: invalid certificate (reason 1)"},
		{"TLS record", tls.RecordHeaderError{Msg: secret}, "TLS: invalid record header"},
		{"deadline", context.DeadlineExceeded, "context deadline exceeded"},
		{"network timeout", os.ErrDeadlineExceeded, "network timeout"},
		{"canceled", context.Canceled, "context canceled"},
		{"truncated", io.ErrUnexpectedEOF, "unexpected EOF"},
		{"EOF", io.EOF, "EOF"},
		{"closed", net.ErrClosed, "use of closed network connection"},
		{"SDK unavailable", racersdk.ErrUnavailable, racersdk.ErrUnavailable.Error()},
		{"SDK version", racersdk.ErrVersionMismatch, racersdk.ErrVersionMismatch.Error()},
		{"unknown", errors.New(secret), "unrecognized cause (*errors.errorString); details redacted"},
		{"oversized", errors.New(strings.Repeat(secret, 4096)), "unrecognized cause (*errors.errorString); details redacted"},
		{"joined", errors.Join(errors.New(secret), syscall.ECONNRESET), "connection reset by peer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.err
			if err != nil {
				// Match the pull, HTTP, and socket wrappers without trusting their text.
				err = &pullFailure{reason: failureTransport, err: fmt.Errorf("%s: %w", secret,
					&url.Error{Op: secret, URL: secret, Err: &net.OpError{Op: secret, Net: secret, Err: err}})}
			}

			require.Equal(t, test.want, failureDiagnostic(err))

			if test.err != nil {
				require.ErrorIs(t, err, test.err, "diagnostics must not alter the returned error chain")
			}
		})
	}
}

func TestPullFailureLogsUnderlyingCause(t *testing.T) {
	for _, cause := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET} {
		t.Run(cause.Error(), func(t *testing.T) {
			p := &puller{metrics: pullTestMetrics()}

			var output bytes.Buffer

			logger := slog.New(slog.NewJSONHandler(&output, nil))
			err := &pullFailure{reason: failureTransport, kind: "blob", err: &url.Error{
				Op: "Get", URL: "https://user:password@private.invalid/path?token=secret", Err: cause,
			}}
			p.reportPullFailure(err, time.Now(), logger)

			var record struct {
				Reason string `json:"reason"`
				Error  string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			require.Equal(t, "transport", record.Reason)
			require.Equal(t, cause.Error(), record.Error)

			for _, secret := range []string{"password", "private.invalid", "path", "token", "secret"} {
				require.NotContains(t, output.String(), secret)
			}
		})
	}
}

func TestFailureDiagnosticHTTPAcquisition(t *testing.T) {
	opts := pullTestOptions("http://private.invalid")
	opts.Namespace = "token=secret"
	img := pullTestImage(t)
	p, _ := pullTestNew(t, img, opts)
	p.transport.Proxy = nil
	p.transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	}

	err := p.fetch(t.Context(), "manifest", img.Manifest)
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
	require.Contains(t, err.Error(), "secret", "the original HTTP error contains the namespace URL query")
	require.Equal(t, "connection refused", failureDiagnostic(err))
}

func TestFailureDiagnosticS3ServerText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>token=secret</Message></Error>")
	}))
	t.Cleanup(server.Close)
	catalog := s3TestCatalog(t, 1, 123)
	p, _ := s3TestPuller(t, catalog, server.URL)
	err := p.fetch(t.Context(), "blob", catalog.batches[0].blobs[0].descriptor)
	require.Error(t, err)
	require.Contains(t, err.Error(), "token=secret", "the SDK preserves untrusted server text")
	require.Equal(t, failureStatus, classifyFailure(err))
	require.Equal(t, "unrecognized cause (*smithy.GenericAPIError); details redacted", failureDiagnostic(err))
}

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

func TestPullIntegrityEvidenceLogs(t *testing.T) {
	expected := digest.FromString("expected synthetic bytes").String()
	actual := digest.FromString("received synthetic bytes").String()

	secret := "https://user:password@private.invalid/payload?token=secret\nforged"
	for _, test := range []struct {
		name     string
		expected string
		actual   string
		want     string
	}{
		{"valid", expected, actual, expected},
		{"URL", secret, secret, "invalid"},
		{"wrong algorithm", "sha512:" + strings.Repeat("a", 128), secret, "invalid"},
		{"invalid hex", "sha256:" + strings.Repeat("z", 64), secret, "invalid"},
		{"oversized", strings.Repeat("a", 4096), secret, "invalid"},
		{"empty", "", "", "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			p := &puller{metrics: newMetrics(reg)}

			var output bytes.Buffer

			logger := slog.New(slog.NewJSONHandler(&output, nil))
			now := time.Now()
			failure := &pullFailure{
				err: errors.New(secret), reason: failureDigest, kind: "layer", status: http.StatusOK,
				integrity: &integrityEvidence{
					expectedDigest: test.expected, actualDigest: test.actual,
					expectedSize: 16777217, receivedSize: 16777217,
				},
			}
			wrapped := fmt.Errorf("%s: %w", secret, failure)

			p.reportPullFailure(nil, now, logger)
			require.Empty(t, output.String())
			p.reportPullFailure(wrapped, now, logger)

			first := output.String()

			for range 100 {
				p.reportPullFailure(wrapped, now, logger)
			}

			require.Equal(t, first, output.String(), "integrity evidence obeys the existing rate limit")
			require.Less(t, len(first), 1024, "evidence size is independent of untrusted input length")

			var record struct {
				Reason    string         `json:"reason"`
				Integrity map[string]any `json:"integrity"`
			}
			require.NoError(t, json.Unmarshal([]byte(first), &record))

			wantActual := "invalid"
			if test.name == "valid" {
				wantActual = actual
			}

			require.Equal(t, "digest_mismatch", record.Reason)
			require.Equal(t, map[string]any{
				"content_digest": test.want, "expected_digest": test.want, "actual_digest": wantActual,
				"expected_size": float64(16777217), "received_size": float64(16777217),
			}, record.Integrity)

			for _, text := range []string{"password", "private.invalid", "payload", "token", "secret", "forged"} {
				require.NotContains(t, first, text)
			}

			p.reportPullFailure(wrapped, now.Add(failureLogInterval), logger)
			require.Equal(t, 2, strings.Count(output.String(), "\n"))
			require.Contains(t, output.String(), `"suppressed":100`)
			family := gatherLoadgenMetrics(t, reg)["racer_loadgen_pull_failures_total"]
			require.Len(t, family.GetMetric(), 1)
			metric := metricWithLabels(t, family, map[string]string{"reason": "digest_mismatch"})
			require.Len(t, metric.GetLabel(), 1, "object identity must not become a metric label")
			require.Equal(t, float64(102), metric.GetCounter().GetValue())
		})
	}
}

func TestPullIntegrityEvidenceNotLoggedForOtherFailures(t *testing.T) {
	p := &puller{metrics: pullTestMetrics()}

	var output bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&output, nil))
	for _, err := range []error{errors.New("digest mismatch: secret"), &pullFailure{
		err: context.DeadlineExceeded, reason: failureDigest, kind: "layer", status: 200,
		integrity: &integrityEvidence{expectedDigest: digest.FromString("expected").String()},
	}} {
		p.reportPullFailure(err, time.Now(), logger)
	}

	require.Equal(t, 2, strings.Count(output.String(), "\n"))
	require.NotContains(t, output.String(), `"integrity"`)
	require.NotContains(t, output.String(), "secret")
}
