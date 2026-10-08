// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/metrics"
)

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	sizes     []int
	writeErr  error
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))
	if w.writeErr != nil {
		return 0, w.writeErr
	}

	return w.ResponseRecorder.Write(p)
}

func TestRacerIOBoundedWritesAndCancellation(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	wrapped := &racerResponseWriter{ResponseWriter: w, timeout: time.Second}
	data := bytes.Repeat([]byte("x"), 3*racerWriteChunk+1)

	n, err := wrapped.Write(data)
	if err != nil || n != len(data) || !bytes.Equal(w.Body.Bytes(), data) || len(w.sizes) != 4 {
		t.Fatalf("bytes=%d sizes=%v err=%v", n, w.sizes, err)
	}

	for _, size := range w.sizes {
		if size > racerWriteChunk {
			t.Fatal("unbounded write")
		}
	}

	if err := wrapped.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := wrapped.SetWriteDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := wrapped.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("lost cancellation: %v", err)
	}

	if err := wrapped.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	if _, err := wrapped.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
}

func TestRacerHTTPObservationAndProgress(t *testing.T) {
	observed := make(chan HTTPObservation, 1)
	server := httptest.NewUnstartedServer(WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)

		_, _ = io.WriteString(w, "abc")

		time.Sleep(100 * time.Millisecond)

		_, _ = io.WriteString(w, "def")
	}), 30*time.Millisecond, func(o HTTPObservation) { observed <- o }))
	server.EnableHTTP2 = true

	server.StartTLS()
	defer server.Close()

	server.Client().Timeout = 3 * time.Second

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil || string(body) != "abcdef" || resp.ProtoMajor != 2 {
		t.Fatalf("body=%q err=%v", body, err)
	}

	select {
	case o := <-observed:
		if o.Aborted || o.Bytes != 6 || o.Status != 200 {
			t.Fatalf("observation=%+v", o)
		}
	case <-time.After(time.Second):
		t.Fatal("missing observation")
	}
}

func TestExtractionRacerIOStalledDownstream(t *testing.T) {
	done := make(chan error, 1)

	server := httptest.NewServer(WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := make([]byte, racerWriteChunk)
		for {
			if _, err := w.Write(data); err != nil {
				done <- err
				return
			}
		}
	}), 50*time.Millisecond, nil))
	defer server.Close()

	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled downstream not interrupted")
	}
}

func TestRacerLimitedListener(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	listener := LimitListener(base, 1)
	defer listener.Close()

	client, err := net.DialTimeout("tcp", base.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, ok := conn.(io.ReaderFrom); !ok {
		t.Fatal("TCP ReaderFrom hidden")
	}

	done := make(chan error, 1)

	go func() { _, err := listener.Accept(); done <- err }()

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock admission")
	}
}

func TestExtractionRacerProductionMetrics(t *testing.T) {
	reg := metrics.New()
	m := NewMetrics(reg)
	m.MirrorResponse(HTTPObservation{Method: http.MethodGet, Status: 206, Bytes: 11, Duration: time.Second, Aborted: true})
	m.OriginRequest(http.MethodHead, 200)
	m.OriginBytes("blob", 17)

	if testutil.ToFloat64(m.requests.WithLabelValues("GET", "206", "aborted")) != 1 || testutil.ToFloat64(m.bytes.WithLabelValues("GET")) != 11 || testutil.ToFloat64(m.originRequests.WithLabelValues("HEAD", "200")) != 1 || testutil.ToFloat64(m.originBodyBytes.WithLabelValues("blob")) != 17 {
		t.Fatal("incorrect accounting")
	}

	families, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "gantry_racer_sdk_") {
			t.Fatal("unexpected SDK metric")
		}
	}
}

type fastWriter struct {
	*deadlineWriter
	reads int
}

func (w *fastWriter) ReadFrom(r io.Reader) (int64, error) {
	w.reads++
	return io.Copy(w.ResponseRecorder, r)
}

func TestExtractionRacerIOBoundedReadFrom(t *testing.T) {
	for _, missing := range []int64{0, 1} {
		w := &fastWriter{deadlineWriter: &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
		wrapped := &racerResponseWriter{ResponseWriter: w, timeout: time.Second}
		length := 3*racerSocketChunk + 1
		source := &io.LimitedReader{R: bytes.NewReader(make([]byte, length)), N: int64(length) + missing}

		n, err := wrapped.ReadFrom(source)
		if n != int64(length) || w.reads != 4 || source.N != missing || wrapped.bytes != n || wrapped.failed != (missing != 0) {
			t.Fatalf("bytes=%d reads=%d remaining=%d failed=%v err=%v", n, w.reads, source.N, wrapped.failed, err)
		}

		if missing != 0 && !errors.Is(err, io.ErrUnexpectedEOF) || missing == 0 && err != nil {
			t.Fatal(err)
		}
	}
}

func TestRacerIOFailureObservation(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: errors.New("failed")}

	var observed HTTPObservation

	handler := WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("body")); err != nil {
			panic(http.ErrAbortHandler)
		}
	}), time.Second, func(o HTTPObservation) { observed = o })

	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("missing abort")
			}
		}()

		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if !observed.Aborted || observed.Bytes != 0 {
		t.Fatalf("observation=%+v", observed)
	}
}
