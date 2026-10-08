// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/pkg/racersdk"
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
	t.Run("nil client correlation", func(t *testing.T) {
		var logs bytes.Buffer

		handler := NewHandler(nil, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
		w := httptest.NewRecorder()
		handler.ServeContent(w, httptest.NewRequest(http.MethodGet, "/", nil), ifaces.OriginRef{})

		entry := decodeRacerDiagnostic(t, &logs)

		id := w.Header().Get("Gantry-Racer-Request-ID")
		if w.Code != 503 || w.Body.String() != "Racer unavailable\n" || len(id) < 26 || entry["request_id"] != id || entry["subsystem"] != "mirror" || entry["stage"] != "client" {
			t.Fatalf("unavailable response=%d %q diagnostic=%v", w.Code, w.Body.String(), entry)
		}

		if NewHandler(nil, nil, nil).logger == nil {
			t.Fatal("nil logger did not select default")
		}
	})

	const secret = "https://user:password@private.example/repo?token=credential"
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{"wrapped", fmt.Errorf("%s: %w", secret, fmt.Errorf("%w: %s", racersdk.ErrUnavailable, secret)), "unavailable"},
		{"truncated", fmt.Errorf("%w: %w", racersdk.ErrUnavailable, io.ErrUnexpectedEOF), "unexpected EOF"},
		{"not found", fmt.Errorf("%s: %w", secret, racersdk.ErrNotFound), "not found"},
		{"raw", errors.New(secret), "unclassified"},
		{"canceled", fmt.Errorf("%s: %w", secret, context.Canceled), "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"closed", net.ErrClosed, "closed"},
		{"EOF", io.ErrUnexpectedEOF, "unexpected EOF"},
		{"short", io.ErrShortWrite, "short write"},
		{"length", nil, "length mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer

			s := NewHandler(nil, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
			s.logRacerFailure("generated-id", "write_body", tc.err, 100, 42)

			entry := decodeRacerDiagnostic(t, &logs)
			if entry["error_kind"] != tc.kind || entry["expected_bytes"] != float64(100) || entry["written_bytes"] != float64(42) {
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
	Client
	err     error
	statErr bool
}

func (c racerDiagnosticClient) Stat(ctx context.Context, req racersdk.Request) (racersdk.Metadata, error) {
	if c.statErr {
		return racersdk.Metadata{}, c.err
	}

	return c.Client.Stat(ctx, req)
}

func (c racerDiagnosticClient) Get(ctx context.Context, req racersdk.Request, opts ...racersdk.ReadOptions) (*racersdk.Object, error) {
	if c.err != nil {
		return nil, c.err
	}

	return c.Client.Get(ctx, req, opts...)
}

type racerDiagnosticFlushWriter struct{ *httptest.ResponseRecorder }

func (w racerDiagnosticFlushWriter) FlushError() error { return errors.New("flush error") }

func TestRacerFailureDiagnosticsPaths(t *testing.T) {
	for _, tc := range []struct {
		name, method, stage                            string
		ranged, failGet, failStat, failBody, failFlush bool
		status                                         int
		expected                                       int64
	}{
		{name: "success", method: http.MethodGet, status: 200},
		{name: "head success", method: http.MethodHead, status: 200},
		{name: "get", method: http.MethodGet, failGet: true, stage: "get", status: 503, expected: -1},
		{name: "stat", method: http.MethodHead, failStat: true, stage: "stat", status: 503, expected: -1},
		{name: "ranged get", method: http.MethodGet, ranged: true, failGet: true, stage: "get", status: 503, expected: 2},
		{name: "flush", method: http.MethodGet, failFlush: true, stage: "flush_headers", status: 200, expected: 3},
		{name: "body", method: http.MethodGet, failBody: true, stage: "write_body", status: 200, expected: 1 << 20},
		{name: "ranged body", method: http.MethodGet, ranged: true, failBody: true, stage: "write_body", status: 206, expected: 1<<20 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

			metadata := racerMetadata(t, d, 3)
			if tc.failBody {
				metadata.Size = 1 << 20
			}

			client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if req.Head {
					return metadata, nil, nil
				}

				if tc.failBody {
					return metadata, io.NopCloser(strings.NewReader(strings.Repeat("a", 1<<19))), nil
				}

				return metadata, io.NopCloser(strings.NewReader("abc")), nil
			})

			wrapped := racerDiagnosticClient{Client: client, statErr: tc.failStat}
			if tc.failGet || tc.failStat {
				wrapped.err = fmt.Errorf("SDK error: %w", racersdk.ErrUnavailable)
			}

			var logs bytes.Buffer

			s := NewHandler(wrapped, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
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

				ref := ifaces.OriginRef{Registry: "registry.example", Repository: "repo", Digest: d, Kind: ifaces.KindBlob}
				if tc.ranged {
					ref.Offset = 1
				}

				s.ServeContent(w, r, ref)
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
	client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{}, nil, fmt.Errorf("%w: secret origin URL", racersdk.ErrUnavailable)
	})

	var logs bytes.Buffer

	s := NewHandler(client, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.ServeContent(w, r, ifaces.OriginRef{Registry: "registry.example", Repository: "repo", Digest: d, Kind: ifaces.KindBlob})

	entry := decodeRacerDiagnostic(t, &logs)
	if w.Code != http.StatusServiceUnavailable || entry["error_kind"] != "unavailable" {
		t.Fatalf("lost SDK response error: status=%d diagnostic=%v", w.Code, entry)
	}

	if strings.Contains(logs.String(), "secret") {
		t.Fatal("origin cause leaked")
	}
}

// Deliberately hides ReaderFrom while preserving ResponseController operations.
type racerHTTPFallback struct{ http.ResponseWriter }

func (w racerHTTPFallback) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func handlerStreamHandler(t *testing.T, client Client, timeout time.Duration, observe func(HTTPObservation)) http.Handler {
	t.Helper()

	trap := &racerLegacyTrap{}

	t.Cleanup(func() {
		if trap.storeCalls.Load() != 0 || trap.originCalls.Load() != 0 {
			t.Error("stream reached legacy content backend")
		}
	})

	return WrapHTTP(mirror.New(racerConfig(), trap, trap, mirror.WithContentBackend(NewHandler(client, nil, nil))).Handler(), timeout, observe)
}

// Keep HTTP/1 reuse and fallback coverage identical across the stream tests.
func handlerStreamServer(t *testing.T, mode string, handler http.Handler) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var connections atomic.Int32

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "fallback" {
			w = racerHTTPFallback{w}
		}

		handler.ServeHTTP(w, r)
	}))

	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	if mode == "TLS" {
		server.StartTLS()
	} else {
		server.Start()
	}

	t.Cleanup(server.Close)
	server.Client().Timeout = 10 * time.Second

	return server, &connections
}

func handlerAwaitObservation(t *testing.T, observed <-chan HTTPObservation) HTTPObservation {
	t.Helper()

	select {
	case observation := <-observed:
		return observation
	case <-time.After(5 * time.Second):
		t.Fatal("missing HTTP observation")
		return HTTPObservation{}
	}
}

func handlerRange(offset int) (string, int) {
	if offset == 0 {
		return "", http.StatusOK
	}

	return fmt.Sprintf("bytes=%d-", offset), http.StatusPartialContent
}

// Each block depends on both the object and its absolute offset, rather than
// repeating a pattern at copy-chunk or page boundaries.
func racerDistinctPayload(object uint64, size int) []byte {
	data := make([]byte, size)

	var seed [16]byte
	binary.LittleEndian.PutUint64(seed[:8], object)

	for offset := 0; offset < size; offset += sha256.Size {
		binary.LittleEndian.PutUint64(seed[8:], uint64(offset))
		block := sha256.Sum256(seed[:])
		copy(data[offset:], block[:])
	}

	return data
}

func racerDistinctOrigin(t *testing.T, objects ...[]byte) racersdk.Origin {
	t.Helper()

	origins := make(map[racersdk.Key]racersdk.Origin, len(objects))
	for _, data := range objects {
		d := racerDigest(data)

		key, err := racersdk.ParseKey(d.Hex())
		if err != nil {
			t.Fatal(err)
		}

		origins[key] = racerPageOrigin(t, d, data)
	}

	return func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		origin, ok := origins[req.Key]
		if !ok {
			return racersdk.Metadata{}, nil, errors.New("unexpected object key")
		}

		return origin(ctx, req)
	}
}

// Hash the bytes actually consumed, independently of the response's digest
// headers and SDK metadata. Each invocation owns its hash state.
func racerCheckStreamHash(t *testing.T, resp *http.Response, data []byte, offset int) {
	t.Helper()

	defer resp.Body.Close()

	want := sha256.Sum256(data[offset:])
	hash := sha256.New()

	n, err := io.Copy(hash, resp.Body)
	if err != nil || n != int64(len(data)-offset) || !bytes.Equal(hash.Sum(nil), want[:]) {
		t.Fatalf("offset=%d bytes=%d want=%d sha256=%x want=%x err=%v", offset, n, len(data)-offset, hash.Sum(nil), want, err)
	}
}

func TestRacerStreamingHTTPReuse(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			objects := [][]byte{racerDistinctPayload(1, int(racersdk.PageSize)+65539), racerDistinctPayload(2, int(racersdk.PageSize)+65539)}
			client := racerFakeClient(t, racerDistinctOrigin(t, objects...))
			observed := make(chan HTTPObservation, 1)
			handler := handlerStreamHandler(t, client, time.Second, func(o HTTPObservation) { observed <- o })
			server, connections := handlerStreamServer(t, mode, handler)

			for _, offset := range []int{0, int(racersdk.PageSize) - 7, int(racersdk.PageSize), len(objects[0]) - 1} {
				for _, data := range objects {
					d := racerDigest(data)

					rangeHeader, status := handlerRange(offset)

					resp := racerRequest(t, server, http.MethodGet, "blobs", d, rangeHeader, "")
					racerCheckStreamHash(t, resp, data, offset)

					if resp.StatusCode != status || resp.ContentLength != int64(len(data)-offset) || resp.ProtoMajor != 1 || resp.Close {
						t.Fatalf("offset=%d status=%d length=%d proto=%s close=%v", offset, resp.StatusCode, resp.ContentLength, resp.Proto, resp.Close)
					}

					if offset != 0 && resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)) {
						t.Fatal("incorrect range headers", resp.Header)
					}

					if o := handlerAwaitObservation(t, observed); o.Aborted || o.Status != status || o.Bytes != int64(len(data)-offset) {
						t.Fatalf("observation=%+v", o)
					}
				}
			}

			if connections.Load() != 1 {
				t.Fatalf("successful full/range responses did not reuse connection: %d", connections.Load())
			}
		})
	}
}

func TestRacerStreamingHTTPTruncation(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		for _, failure := range []string{"mid-page", "terminal origin error"} {
			for _, offset := range []int{0, 65536} {
				t.Run(fmt.Sprintf("%s/%s/%d", mode, failure, offset), func(t *testing.T) {
					data := racerDistinctPayload(3, 1024*1024)
					d := racerDigest(data)
					metadata := racerMetadata(t, d, len(data))
					client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
						if req.Head {
							return metadata, nil, nil
						}

						payload := data
						if failure == "mid-page" {
							payload = data[:len(data)/2]
						}
						// A terminal origin error prevents the fake from sending Complete.
						// ServeOrigin also gates its final byte, so malformed Complete
						// after a full page is covered by the SDK's raw protocol tests.
						return metadata, io.NopCloser(io.MultiReader(bytes.NewReader(payload), racerReadError{})), nil
					})
					observed := make(chan HTTPObservation, 1)
					handler := handlerStreamHandler(t, client, time.Second, func(o HTTPObservation) { observed <- o })
					server, _ := handlerStreamServer(t, mode, handler)
					rangeHeader, status := handlerRange(offset)

					resp := racerRequest(t, server, http.MethodGet, "blobs", d, rangeHeader, "")
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()

					if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) == 0 || len(got) >= len(data)-offset || !bytes.Equal(got, data[offset:offset+len(got)]) || resp.StatusCode != status {
						t.Fatalf("failed stream: status=%d bytes=%d err=%v", resp.StatusCode, len(got), err)
					}

					if o := handlerAwaitObservation(t, observed); !o.Aborted || o.Status != status || o.Bytes < int64(len(got)) || o.Bytes >= int64(len(data)-offset) {
						t.Fatalf("observation=%+v", o)
					}
				})
			}
		}
	}
}

// Hold an actual downstream Write until released or canceled. This makes
// backpressure deterministic without sleeps or assumptions about socket buffers.
// The held streams use Write; unheld streams retain their normal ReaderFrom path.
type racerHeldResponse struct {
	http.ResponseWriter
	ctx     context.Context
	entered chan struct{}
	release <-chan struct{}
}

func (w *racerHeldResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *racerHeldResponse) Write(p []byte) (int, error) {
	if w.entered != nil {
		close(w.entered)

		w.entered = nil
		select {
		case <-w.release:
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		}
	}

	return w.ResponseWriter.Write(p)
}

func racerAwaitStream(t *testing.T, event <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestRacerStreamingHTTPConcurrentDistinctObjects(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			objects := [][]byte{racerDistinctPayload(11, 2*int(racersdk.PageSize)+65539), racerDistinctPayload(12, 2*int(racersdk.PageSize)+65539)}
			client := racerFakeClient(t, racerDistinctOrigin(t, objects...))
			entered := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			done := [2]chan struct{}{make(chan struct{}), make(chan struct{})}

			var aborted atomic.Int32

			handler := handlerStreamHandler(t, client, 10*time.Second, func(o HTTPObservation) {
				if o.Aborted {
					aborted.Add(1)
				}
			})

			server, _ := handlerStreamServer(t, mode, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hold := r.Header.Get("X-Test-Hold"); hold != "" {
					i := 0
					if hold == "slow" {
						i = 1
					}
					defer close(done[i])

					w = &racerHeldResponse{ResponseWriter: w, ctx: r.Context(), entered: entered[i], release: release[i]}
				}

				handler.ServeHTTP(w, r)
			}))
			server.Client().Timeout = 20 * time.Second

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// Ensure a failed assertion also unblocks the slow writer before Close.
			slowCtx, stopSlow := context.WithCancel(t.Context())
			defer stopSlow()

			startHeld := func(ctx context.Context, d digest.Digest, hold string) *http.Response {
				req := handlerRequest(t, ctx, server, http.MethodGet, "blobs", d)
				req.Header.Set("X-Test-Hold", hold)

				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { resp.Body.Close() })

				if resp.StatusCode != http.StatusOK {
					t.Fatalf("held response status=%d", resp.StatusCode)
				}

				return resp
			}
			victim := startHeld(ctx, racerDigest(objects[0]), "cancel")
			slow := startHeld(slowCtx, racerDigest(objects[1]), "slow")

			racerAwaitStream(t, entered[0], "cancelable downstream write")
			racerAwaitStream(t, entered[1], "backpressured downstream write")

			// A third stream completes while both earlier streams retain buffers.
			resp := racerRequest(t, server, http.MethodGet, "blobs", racerDigest(objects[1]), "", "")
			racerCheckStreamHash(t, resp, objects[1], 0)
			cancel()

			_, err := io.Copy(io.Discard, victim.Body)
			victim.Body.Close()

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled read: %v", err)
			}

			racerAwaitStream(t, done[0], "canceled handler cleanup")

			if aborted.Load() != 1 {
				t.Fatalf("abort count=%d", aborted.Load())
			}

			// Reuse released capacity for the other object at an unaligned page
			// boundary, then read the previously backpressured object to completion.
			offset := int(racersdk.PageSize) - 7

			resp = racerRequest(t, server, http.MethodGet, "blobs", racerDigest(objects[0]), fmt.Sprintf("bytes=%d-", offset), "")
			if resp.StatusCode != http.StatusPartialContent {
				t.Fatalf("range status=%d", resp.StatusCode)
			}

			racerCheckStreamHash(t, resp, objects[0], offset)
			close(release[1])
			racerCheckStreamHash(t, slow, objects[1], 0)
			racerAwaitStream(t, done[1], "slow handler completion")
			resp = racerRequest(t, server, http.MethodGet, "blobs", racerDigest(objects[0]), "", "")
			racerCheckStreamHash(t, resp, objects[0], 0)

			if aborted.Load() != 1 {
				t.Fatalf("healthy stream aborted: %d", aborted.Load())
			}
		})
	}
}
