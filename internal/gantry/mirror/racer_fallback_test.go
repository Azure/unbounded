// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

type fallbackRegistry struct {
	metadataOnlyRegistry
	body io.ReadCloser
	size int64
}

func (o *fallbackRegistry) PullWithMetadata(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	return o.body, o.size, "", nil
}

type fallbackBody struct {
	reader   *bytes.Reader
	terminal error
	withData bool
	closed   bool
}

func (b *fallbackBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF || b.withData && b.reader.Len() == 0 {
		return n, b.terminal
	}

	return n, err
}

func (b *fallbackBody) Close() error {
	b.closed = true
	return nil
}

func TestRacerFallbackFramingAndCompletion(t *testing.T) {
	data := bytes.Repeat([]byte("forwarded bytes!"), 8192)
	size := int64(len(data))
	failure := errors.New("origin transport failed")

	for _, tc := range []struct {
		name     string
		data     []byte
		size     int64
		terminal error
		withData bool
		failed   bool
	}{
		{"known-size", data, size, io.EOF, false, false},
		{"unknown-size", data, -1, io.EOF, false, false},
		{"empty-known-size", nil, 0, io.EOF, false, false},
		{"empty-unknown-size", nil, -1, io.EOF, false, false},
		{"short", data, size + 1, io.EOF, false, true},
		{"empty-short", nil, 1, io.EOF, false, true},
		{"long", data, size - 1, io.EOF, false, true},
		// Exactly one copy buffer is followed by an extra byte. Content-Length
		// framing could hide that byte and make an aborted transfer look valid.
		{"overrun-after-full-buffer", data[:32769], 32768, io.EOF, false, true},
		{"zero-size-overrun", data[:1], 0, io.EOF, false, true},
		{"small-overrun", data[:2], 1, io.EOF, false, true},
		{"initial-error", nil, 0, failure, false, true},
		{"unknown-initial-error", nil, -1, failure, false, true},
		{"late-error-at-size", data, size, failure, false, true},
		{"small-late-error-at-size", data[:1], 1, failure, false, true},
		{"unknown-late-error", data, -1, failure, false, true},
		{"data-and-error", data, size, failure, true, true},
		{"data-and-eof", data, size, io.EOF, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &fallbackBody{reader: bytes.NewReader(tc.data), terminal: tc.terminal, withData: tc.withData}
			up := &fallbackRegistry{body: body, size: tc.size}
			// Intentionally unrelated, including for empty bodies: Gantry must
			// not verify this digest or report a containerd commit.
			d := digestOf([]byte("independent expected OCI digest"))

			var (
				started, completed, failed, responses, live int
				served                                      int64
			)

			server := mirror.NewRacer(reviewConfig(), fakes.NewCache(), up, nil,
				mirror.WithOriginStreamMetrics(func(string) { started++ }, func(string) { completed++ }, func(string) { failed++ }),
				mirror.WithByteMetrics(func(_, source string, n int64) {
					if source != "origin" {
						t.Error("wrong byte source", source)
					}

					served += n
				}),
				mirror.WithMirrorResponseCompletedHook(func(got digest.Digest, _, source string) {
					if got != d || source != "origin" {
						t.Error("wrong completion identity", got, source)
					}

					responses++
				}),
				mirror.WithLiveStreamCompletedHook(func(digest.Digest) { live++ }))
			finished := make(chan struct{})
			handler := server.Handler()

			m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)

				handler.ServeHTTP(w, r)
			}))
			defer m.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, m.URL+"/v2/repo/blobs/"+d.String(), nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Range", "bytes=1-2")

			resp, err := m.Client().Do(req)
			if err != nil {
				t.Fatal("headers should precede body failure", err)
			}

			got, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			<-finished

			if resp.StatusCode != http.StatusOK || resp.ContentLength != -1 || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" || resp.Header.Get("Content-Range") != "" || len(resp.Header.Values("Content-Type")) != 0 {
				t.Fatal("incorrect full-response framing or metadata", resp)
			}

			if !body.closed || started != 1 || served != int64(len(tc.data)) {
				t.Fatal("lost cleanup or byte accounting", body.closed, started, served)
			}

			if tc.failed {
				if readErr == nil || failed != 1 || completed != 0 || responses != 0 || live != 0 {
					t.Fatal("failed forwarding appeared complete", readErr, failed, completed, responses, live)
				}
			} else if readErr != nil || !bytes.Equal(got, tc.data) || failed != 0 || completed != 1 || responses != 1 || live != 1 {
				t.Fatal("forwarding did not complete", readErr, len(got), failed, completed, responses, live)
			}
		})
	}
}

type fallbackFaultWriter struct {
	*httptest.ResponseRecorder
	fault   string
	flushes int
}

func (w *fallbackFaultWriter) SetWriteDeadline(time.Time) error {
	if w.fault == "deadline" {
		return errors.New("deadline failed")
	}

	return nil
}

func (w *fallbackFaultWriter) FlushError() error {
	w.flushes++
	if w.fault == "flush" || w.fault == "final-flush" && w.flushes == 2 {
		return errors.New("flush failed")
	}

	w.Flush()

	return nil
}

func (w *fallbackFaultWriter) Write(p []byte) (int, error) {
	if w.fault == "write" {
		return 0, errors.New("write failed")
	}

	if w.fault == "short-write" {
		return len(p) - 1, nil
	}

	return w.ResponseRecorder.Write(p)
}

func TestRacerFallbackWriterFailureAborts(t *testing.T) {
	for _, fault := range []string{"deadline", "flush", "final-flush", "write", "short-write", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			body := &fallbackBody{reader: bytes.NewReader([]byte("payload")), terminal: io.EOF}
			up := &fallbackRegistry{body: body, size: 7}

			var completed int

			server := mirror.NewRacer(reviewConfig(), fakes.NewCache(), up, nil,
				mirror.WithLiveStreamCompletedHook(func(digest.Digest) { completed++ }))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if fault == "canceled" {
				cancel()
			}

			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v2/repo/blobs/"+digestOf([]byte("payload")).String(), nil)
			w := &fallbackFaultWriter{ResponseRecorder: httptest.NewRecorder(), fault: fault}

			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Error("failure did not abort framing", got)
				}

				if completed != 0 || fault != "deadline" && !body.closed {
					t.Error("failure reported completion or leaked body", completed, body.closed)
				}
			}()

			server.Handler().ServeHTTP(w, req)
		})
	}
}

func TestRacerFallbackRejectsCloseDelimitedGET(t *testing.T) {
	up := &metadataOnlyRegistry{authorizationCapturingOrigin{seen: make(chan string, 1)}}
	server := mirror.NewRacer(reviewConfig(), fakes.NewCache(), up, nil)
	req := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+digestOf(nil).String(), nil)
	req.Proto, req.ProtoMinor = "HTTP/1.0", 0
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusHTTPVersionNotSupported || len(up.seen) != 0 {
		t.Fatal("close-delimited fallback was allowed", w.Code)
	}
}

func TestRacerFallbackStreamsAndCancelsOrigin(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 64<<10)
	canceled := make(chan struct{})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer up.Close()

	cfg := reviewConfig()
	cfg.UpstreamRegistries[0].Endpoint = up.URL

	registry, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var failed, completed int

	server := mirror.NewRacer(cfg, fakes.NewCache(), registry, nil,
		mirror.WithOriginStreamMetrics(nil, func(string) { completed++ }, func(string) { failed++ }))
	finished := make(chan struct{})
	handler := server.Handler()

	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)

		handler.ServeHTTP(w, r)
	}))
	defer m.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL+"/v2/repo/blobs/"+digestOf(data).String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := m.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read well before upstream EOF, proving forwarding does not buffer the
	// whole object. Closing downstream must interrupt the blocked origin read.
	got := make([]byte, 32<<10)
	if _, err := io.ReadFull(resp.Body, got); err != nil || !bytes.Equal(got, data[:len(got)]) {
		t.Fatal("body did not stream before EOF", err)
	}

	_ = resp.Body.Close()

	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("downstream disconnect did not cancel origin")
	}

	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("canceled fallback did not release handler")
	}

	if failed != 1 || completed != 0 {
		t.Fatal("canceled stream reported completion", failed, completed)
	}
}
