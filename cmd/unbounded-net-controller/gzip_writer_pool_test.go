// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// freshGzipHandler preserves the original handler for behavior and allocation
// comparisons against the reusable-writer path.
func freshGzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}

		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		defer func() { _ = gz.Close() }()

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

func TestGzipHandlerPreservesBehavior(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.URL.Path)

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		switch r.URL.Path {
		case "/empty":
			w.WriteHeader(http.StatusAccepted)
		case "/no-content":
			w.WriteHeader(http.StatusNoContent)
		case "/error":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusCreated)

			for _, chunk := range []string{"response:", r.URL.Path} {
				if _, err := io.WriteString(w, chunk); err != nil {
					t.Error(err)
				}
			}
		}
	})
	fresh, pooled := freshGzipHandler(next), gzipHandler(next)

	for _, encoding := range []string{"", "br", "gzip", "br, gzip", "gzip;q=0", "GZIP", "xgzip"} {
		for _, upgrade := range []string{"", "WebSocket"} {
			for _, path := range []string{"/first", "/empty", "/no-content", "/error", "/second"} {
				t.Run(encoding+"/"+upgrade+path, func(t *testing.T) {
					request := httptest.NewRequest(http.MethodGet, path, nil)
					request.Header.Set("Accept-Encoding", encoding)
					request.Header.Set("Upgrade", upgrade)

					want, got := httptest.NewRecorder(), httptest.NewRecorder()

					for _, recorder := range []*httptest.ResponseRecorder{want, got} {
						recorder.Header().Set("Content-Length", "123")
						recorder.Header().Set("Content-Encoding", "original")
					}

					fresh.ServeHTTP(want, request)
					pooled.ServeHTTP(got, request)

					if got.Code != want.Code || got.Flushed != want.Flushed ||
						!reflect.DeepEqual(got.Header(), want.Header()) || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
						t.Fatalf("response behavior changed: status=%d/%d flush=%t/%t headers=%v/%v", got.Code, want.Code, got.Flushed, want.Flushed, got.Header(), want.Header())
					}
				})
			}
		}
	}
}

func TestGzipHandlerHTTPRoundTrips(t *testing.T) {
	handler := gzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.URL.Path)

		if r.URL.Path == "/error" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		if r.URL.Path == "/no-content" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)

		if r.URL.Path != "/empty" {
			if _, err := io.WriteString(w, strings.Repeat(r.URL.Path, 100)); err != nil {
				t.Error(err)
			}
		}
	}))

	server := httptest.NewServer(handler)
	defer server.Close()

	client := server.Client()
	client.Timeout = 10 * time.Second

	check := func(method, path string) {
		t.Helper()

		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
		if err != nil {
			t.Error(err)
			return
		}

		request.Header.Set("Accept-Encoding", "gzip")

		response, err := client.Do(request)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()

		var reader io.Reader = response.Body

		if method != http.MethodHead && path != "/no-content" {
			compressed, err := gzip.NewReader(response.Body)
			if err != nil {
				t.Error(err)
				return
			}
			defer compressed.Close()

			reader = compressed
		}

		body, err := io.ReadAll(reader)
		if err != nil {
			t.Error(err)
			return
		}

		wantStatus, wantBody := http.StatusCreated, strings.Repeat(path, 100)
		switch path {
		case "/empty":
			wantBody = ""
		case "/no-content":
			wantStatus, wantBody = http.StatusNoContent, ""
		case "/error":
			wantStatus, wantBody = http.StatusNotFound, "not found\n"
		}

		if method == http.MethodHead {
			wantBody = ""
		}

		if response.StatusCode != wantStatus || string(body) != wantBody ||
			response.Header.Get("Content-Encoding") != "gzip" || response.Header.Get("X-Response") != path {
			t.Errorf("round trip %s: status=%d body=%q headers=%v", path, response.StatusCode, body, response.Header)
		}
	}

	for _, path := range []string{"/first", "/different", "/empty", "/no-content", "/error", "/last"} {
		check(http.MethodGet, path)
	}

	check(http.MethodHead, "/head")
	check(http.MethodGet, "/after-head")

	var workers sync.WaitGroup

	for worker := range 12 {
		workers.Go(func() {
			for request := range 4 {
				check(http.MethodGet, fmt.Sprintf("/worker-%d-request-%d", worker, request))
			}
		})
	}

	workers.Wait()
}

func TestGzipWriterPoolBoundedAndDetached(t *testing.T) {
	pool := newGzipWriterPool()
	writers := make([]*gzip.Writer, gzipIdleWriterLimit+3)
	outputs := make([]bytes.Buffer, len(writers))

	for i := range writers {
		writer, err := pool.get(&outputs[i])
		if err != nil {
			t.Fatal(err)
		}

		writers[i] = writer

		if _, err := io.WriteString(writer, strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	for _, writer := range writers {
		pool.put(writer, true)
	}

	if len(pool.idle) != gzipIdleWriterLimit {
		t.Fatalf("idle pool length=%d, want %d", len(pool.idle), gzipIdleWriterLimit)
	}

	// Take ownership of all idle writers before inspecting their detached
	// outputs. Overflow writers are already outside the pool.
	for len(pool.idle) > 0 {
		<-pool.idle
	}

	for i, writer := range writers {
		before := outputs[i].Len()

		if _, err := io.WriteString(writer, "discard this"); err != nil {
			t.Fatal(err)
		}

		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		if outputs[i].Len() != before {
			t.Fatal("released writer retained its response destination")
		}
	}
}

func TestGzipWriterPoolResetsHeadersAndSurvivesGC(t *testing.T) {
	pool := newGzipWriterPool()

	var first, second bytes.Buffer

	writer, err := pool.get(&first)
	if err != nil {
		t.Fatal(err)
	}

	writer.Name, writer.Comment, writer.Extra, writer.OS = "private-name", "private-comment", []byte("private-extra"), 7

	if _, err := io.WriteString(writer, "first body"); err != nil {
		t.Fatal(err)
	}

	pool.put(writer, true)
	runtime.GC()
	runtime.GC()

	reused, err := pool.get(&second)
	if err != nil {
		t.Fatal(err)
	}

	if reused != writer {
		t.Fatal("idle writer was not reused")
	}

	if _, err := io.WriteString(reused, "second body"); err != nil {
		t.Fatal(err)
	}

	pool.put(reused, true)

	reader, err := gzip.NewReader(&second)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "second body" {
		t.Fatalf("reused body: %q %v", body, err)
	}

	if reader.Name != "" || reader.Comment != "" || len(reader.Extra) != 0 || reader.OS != 255 {
		t.Fatalf("reused writer retained gzip headers: %+v", reader.Header)
	}
}

type gzipFailureResponseWriter struct {
	*httptest.ResponseRecorder
	failAt  int
	panicAt int
	writes  int
}

func (w *gzipFailureResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.panicAt {
		panic("gzip test output panic")
	}

	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}

	return w.ResponseRecorder.Write(data)
}

func invokeGzipHandler(handler http.Handler, writer http.ResponseWriter, request *http.Request) (panicValue any) {
	defer func() { panicValue = recover() }()

	handler.ServeHTTP(writer, request)

	return nil
}

func TestGzipHandlerWriteFailuresAndPanics(t *testing.T) {
	for _, test := range []struct {
		name         string
		failAt       int
		panicAt      int
		handlerPanic bool
		beforeWrite  bool
	}{
		{name: "write failure", failAt: 1},
		{name: "close failure", failAt: 2},
		{name: "close panic", panicAt: 2},
		{name: "handler panic", handlerPanic: true},
		{name: "handler panic before write", handlerPanic: true, beforeWrite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var used *gzip.Writer

			var writeErr error

			handler := gzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				used = w.(*gzipResponseWriter).Writer.(*gzip.Writer)

				if test.beforeWrite && r.URL.Path == "/failure" {
					panic("gzip test handler panic")
				}

				_, writeErr = io.WriteString(w, r.URL.Path)

				if test.handlerPanic && r.URL.Path == "/failure" {
					panic("gzip test handler panic")
				}
			}))
			failed := &gzipFailureResponseWriter{
				ResponseRecorder: httptest.NewRecorder(), failAt: test.failAt, panicAt: test.panicAt,
			}
			request := httptest.NewRequest(http.MethodGet, "/failure", nil)
			request.Header.Set("Accept-Encoding", "gzip")
			panicValue := invokeGzipHandler(handler, failed, request)

			wantPanic := ""
			if test.panicAt != 0 {
				wantPanic = "gzip test output panic"
			} else if test.handlerPanic {
				wantPanic = "gzip test handler panic"
			}

			if wantPanic != "" && panicValue != wantPanic || wantPanic == "" && panicValue != nil {
				t.Fatalf("panic behavior changed: got %v, want %q", panicValue, wantPanic)
			}

			if test.failAt == 1 && !errors.Is(writeErr, io.ErrClosedPipe) {
				t.Fatalf("write error was suppressed: %v", writeErr)
			}

			oldWriter, oldWrites := used, failed.writes
			if _, err := io.WriteString(oldWriter, "discard this"); err != nil {
				t.Fatalf("failed writer was not reset: %v", err)
			}

			if err := oldWriter.Close(); err != nil {
				t.Fatal(err)
			}

			if failed.writes != oldWrites {
				t.Fatal("failed or panicking writer retained its response destination")
			}

			recorder := httptest.NewRecorder()
			request = httptest.NewRequest(http.MethodGet, "/healthy", nil)
			request.Header.Set("Accept-Encoding", "gzip")
			handler.ServeHTTP(recorder, request)

			if used == oldWriter || writeErr != nil {
				t.Fatal("failed or panicking writer was returned to the idle pool")
			}

			reader, err := gzip.NewReader(recorder.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()

			body, err := io.ReadAll(reader)
			if err != nil || string(body) != "/healthy" {
				t.Fatalf("failure contaminated the next response: %q %v", body, err)
			}
		})
	}
}

func BenchmarkGzipResponseHandler(b *testing.B) {
	for _, response := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name: "discovery", status: http.StatusOK,
			body: `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"status.net.unbounded-cloud.io/v1alpha1","resources":[{"name":"status/push","singularName":"","namespaced":false,"kind":"NodeStatusPush","verbs":["create"]},{"name":"status/nodews","singularName":"","namespaced":false,"kind":"NodeStatusStream","verbs":["get"]},{"name":"status/json","singularName":"","namespaced":false,"kind":"ClusterStatus","verbs":["get"]},{"name":"token/node","singularName":"","namespaced":false,"kind":"TokenRequest","verbs":["create"]},{"name":"token/viewer","singularName":"","namespaced":false,"kind":"TokenRequest","verbs":["create"]}]}`,
		},
		{name: "not-found", status: http.StatusNotFound, body: "404 page not found\n"},
		{name: "empty", status: http.StatusOK},
	} {
		for _, mode := range []struct {
			name string
			wrap func(http.Handler) http.Handler
		}{
			{name: "fresh", wrap: freshGzipHandler},
			{name: "pooled", wrap: gzipHandler},
		} {
			b.Run(response.name+"/"+mode.name, func(b *testing.B) {
				body := []byte(response.body)
				handler := mode.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(response.status)

					if _, err := w.Write(body); err != nil {
						b.Fatal(err)
					}
				}))
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				request.Header.Set("Accept-Encoding", "gzip")
				handler.ServeHTTP(httptest.NewRecorder(), request)

				b.ReportAllocs()
				b.ResetTimer()

				for b.Loop() {
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, request)

					if recorder.Code != response.status {
						b.Fatal(recorder.Code)
					}
				}
			})
		}
	}
}
