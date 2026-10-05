// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func TestRacerFailureDiagnosticsRateBound(t *testing.T) {
	var (
		d        racerFailureDiagnostics
		admitted atomic.Int32
		wg       sync.WaitGroup
	)

	now := time.Unix(100, 0)

	for range 1000 {
		wg.Go(func() {
			if ok, _ := d.allow(now); ok {
				admitted.Add(1)
			}
		})
	}

	wg.Wait()

	if got := admitted.Load(); got != racerFailureLogBurst {
		t.Fatalf("admitted = %d", got)
	}

	if ok, _ := d.allow(now.Add(time.Minute - time.Nanosecond)); ok {
		t.Fatal("admitted before refill")
	}

	if ok, suppressed := d.allow(now.Add(time.Minute)); !ok || suppressed != 991 {
		t.Fatalf("refill = %v, suppressed = %d", ok, suppressed)
	}

	if ok, suppressed := d.allow(now.Add(time.Minute)); !ok || suppressed != 0 {
		t.Fatalf("suppression count did not reset: %v/%d", ok, suppressed)
	}
}

func TestRacerFailureDiagnosticsRedaction(t *testing.T) {
	const secret = "https://user:password@private.example/repo?token=credential"
	for _, tc := range []struct {
		name            string
		err             error
		kind, operation string
	}{
		{"typed wrapped", fmt.Errorf("%s: %w", secret, racersdk.NewOriginError(racersdk.ErrorUnavailable, errors.New(secret))), "unavailable", "origin"},
		{"raw", errors.New(secret), "unclassified", ""},
		{"canceled", fmt.Errorf("%s: %w", secret, context.Canceled), "canceled", ""},
		{"deadline", context.DeadlineExceeded, "deadline", ""},
		{"EOF", io.ErrUnexpectedEOF, "unexpected EOF", ""},
		{"short", io.ErrShortWrite, "short write", ""},
		{"length", nil, "length mismatch", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer

			s := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			s.logRacerFailure("generated-id", "write_body", tc.err, 100, 42)

			entry := decodeRacerDiagnostic(t, &logs)
			if entry["error_kind"] != tc.kind || entry["sdk_operation"] != tc.operation || entry["expected_bytes"] != float64(100) || entry["written_bytes"] != float64(42) {
				t.Fatalf("unexpected diagnostic: %v", entry)
			}

			if strings.Contains(logs.String(), "private.example") || strings.Contains(logs.String(), "credential") || strings.Contains(logs.String(), "password") {
				t.Fatal("diagnostic leaked error data")
			}
		})
	}
}

func decodeRacerDiagnostic(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}

	return entry
}

type racerDiagnosticClient struct {
	RacerClient
	err     error
	statErr bool
}

func (c racerDiagnosticClient) Stat(ctx context.Context, req racersdk.Request) (racersdk.Metadata, error) {
	if c.statErr {
		return racersdk.Metadata{}, c.err
	}

	return c.RacerClient.Stat(ctx, req)
}

func (c racerDiagnosticClient) GetStreaming(ctx context.Context, req racersdk.Request, opts ...racersdk.ReadOptions) (*racersdk.Value, error) {
	if c.err != nil {
		return nil, c.err
	}

	return c.RacerClient.GetStreaming(ctx, req, opts...)
}

type racerDiagnosticFlushWriter struct{ *httptest.ResponseRecorder }

func (w racerDiagnosticFlushWriter) FlushError() error { return errors.New("secret flush error") }

func TestRacerFailureDiagnosticsPaths(t *testing.T) {
	for _, tc := range []struct {
		name, method, stage                            string
		ranged, failGet, failStat, failBody, failFlush bool
		status                                         int
		expected                                       int64
	}{
		{name: "success", method: http.MethodGet, status: 200},
		{name: "head success", method: http.MethodHead, status: 200},
		{name: "get", method: http.MethodGet, failGet: true, stage: "get_streaming", status: 503, expected: -1},
		{name: "stat", method: http.MethodHead, failStat: true, stage: "stat", status: 503, expected: -1},
		{name: "ranged get", method: http.MethodGet, ranged: true, failGet: true, stage: "get_streaming", status: 503, expected: 2},
		{name: "flush", method: http.MethodGet, failFlush: true, stage: "flush_headers", status: 200, expected: 3},
		{name: "body", method: http.MethodGet, failBody: true, stage: "write_body", status: 200, expected: 1 << 20},
		{name: "ranged body", method: http.MethodGet, ranged: true, failBody: true, stage: "write_body", status: 206, expected: 1<<20 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

			tag, err := racersdk.ParseETag(`"` + d.String() + `"`)
			if err != nil {
				t.Fatal(err)
			}

			metadata := racersdk.Metadata{Size: 3, ETag: tag, ContentType: "application/octet-stream", ExpiresAt: time.Unix(2000000000, 0)}
			if tc.failBody {
				metadata.Size = 1 << 20
			}

			client, cleanup, err := racersdktest.NewClient(func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if req.Operation() == racersdk.OperationHead {
					return metadata, nil, nil
				}

				if tc.failBody {
					return metadata, io.NopCloser(strings.NewReader(strings.Repeat("a", 1<<19))), nil
				}

				return metadata, io.NopCloser(strings.NewReader("abc")), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			wrapped := racerDiagnosticClient{RacerClient: client, statErr: tc.failStat}
			if tc.failGet || tc.failStat {
				wrapped.err = racersdk.NewOriginError(racersdk.ErrorUnavailable, errors.New("secret SDK error"))
			}

			var logs bytes.Buffer

			s := New(&config.Config{}, nil, nil, WithRacer(wrapped), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
			r := httptest.NewRequest(tc.method, "http://private.example/secret?token=secret", nil)
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("Gantry-Racer-Request-ID", "secret-caller-id")

			if tc.ranged {
				r.Header.Set("Range", "bytes=1-")
			}

			recorder := httptest.NewRecorder()

			var w http.ResponseWriter = recorder
			if tc.failFlush {
				w = racerDiagnosticFlushWriter{recorder}
			}

			aborted := false

			func() {
				defer func() {
					if p := recover(); p != nil {
						if p != http.ErrAbortHandler {
							panic(p)
						}

						aborted = true
					}
				}()

				s.serveRacer(w, r, "registry.example", "repo", d, ifaces.KindBlob)
			}()

			if aborted != (tc.failBody || tc.failFlush) || recorder.Code != tc.status {
				t.Fatalf("abort/status = %v/%d", aborted, recorder.Code)
			}

			id := recorder.Header().Get("Gantry-Racer-Request-ID")
			if len(id) < 26 || len(id) > 64 || strings.Contains(id, "secret") {
				t.Fatalf("invalid generated ID: %q", id)
			}

			if tc.stage == "" {
				if logs.Len() != 0 {
					t.Fatalf("success logged failure: %s", &logs)
				}

				want := "abc"
				if tc.method == http.MethodHead {
					want = ""
				}

				if recorder.Body.String() != want {
					t.Fatalf("success body = %q; want %q", recorder.Body.String(), want)
				}

				return
			}

			entry := decodeRacerDiagnostic(t, &logs)

			written := 0
			if aborted {
				written = recorder.Body.Len()
			}

			if tc.failBody && !tc.ranged && (written == 0 || int64(written) >= tc.expected) {
				t.Fatalf("expected partial payload, got %d bytes", written)
			}

			if entry["request_id"] != id || entry["stage"] != tc.stage || entry["expected_bytes"] != float64(tc.expected) || entry["written_bytes"] != float64(written) {
				t.Fatalf("diagnostic mismatch: %v", entry)
			}

			if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "private.example") {
				t.Fatal("request/error leaked")
			}
		})
	}
}

func TestRacerFailureDiagnosticsSDKResponse(t *testing.T) {
	client, cleanup, err := racersdktest.NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorUnavailable, errors.New("secret origin URL"))
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	var logs bytes.Buffer

	s := New(&config.Config{}, nil, nil, WithRacer(client), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.serveRacer(w, r, "registry.example", "repo", d, ifaces.KindBlob)

	entry := decodeRacerDiagnostic(t, &logs)
	if w.Code != http.StatusServiceUnavailable || entry["error_kind"] != "unavailable" || entry["sdk_status"] != float64(503) || entry["sdk_operation"] != "response" {
		t.Fatalf("lost typed SDK response: status=%d diagnostic=%v", w.Code, entry)
	}

	if strings.Contains(logs.String(), "secret") {
		t.Fatal("origin cause leaked")
	}
}
