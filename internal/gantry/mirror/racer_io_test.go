// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"
)

type racerDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines   []time.Time
	sizes       []int
	writeErr    error
	deadlineErr error
}

type racerFastWriter struct {
	*racerDeadlineWriter
	reads int
}

func (w *racerFastWriter) ReadFrom(r io.Reader) (int64, error) {
	w.reads++
	return io.Copy(w.ResponseRecorder, r)
}

func TestRacerIOBoundedReadFrom(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		w := &racerFastWriter{racerDeadlineWriter: &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
		wrapped := &racerResponseWriter{ResponseWriter: w, timeout: time.Second}
		length := 3*racerSocketChunk + 1

		advertised := length
		if truncated {
			advertised++
		}

		source := &io.LimitedReader{R: bytes.NewReader(make([]byte, length)), N: int64(advertised)}

		n, err := wrapped.ReadFrom(source)
		if n != int64(length) || truncated != errors.Is(err, io.ErrUnexpectedEOF) || wrapped.failed != truncated {
			t.Fatalf("bytes=%d err=%v failed=%v", n, err, wrapped.failed)
		}

		if w.reads != 4 || len(w.deadlines) != 10 || wrapped.bytes != n || source.N != int64(advertised-length) {
			t.Fatalf("reads=%d deadlines=%d accounted=%d remaining=%d", w.reads, len(w.deadlines), wrapped.bytes, source.N)
		}
	}
}

func (w *racerDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return w.deadlineErr
}

func (w *racerDeadlineWriter) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))
	if w.writeErr != nil {
		return 0, w.writeErr
	}

	return w.ResponseRecorder.Write(p)
}

func TestRacerIOBoundedWritesAndFlush(t *testing.T) {
	const batch = 256 * 1024

	w := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	data := bytes.Repeat([]byte("xyz"), batch+1)[:3*batch+1]

	var observed RacerHTTPObservation

	handler := RacerHTTPHandler(http.HandlerFunc(func(wrapped http.ResponseWriter, _ *http.Request) {
		if !errors.Is(http.NewResponseController(wrapped).SetReadDeadline(time.Now()), http.ErrNotSupported) {
			t.Fatal("unexpected controller behavior")
		}

		if unwrapped := wrapped.(interface{ Unwrap() http.ResponseWriter }).Unwrap(); unwrapped != w {
			t.Fatal("wrong underlying writer")
		}

		wrapped.WriteHeader(http.StatusPartialContent)

		if _, err := io.Copy(wrapped, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}

		wrapped.(http.Flusher).Flush()
	}), 0, func(o RacerHTTPObservation) { observed = o })
	before := time.Now()

	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if !slices.Equal(w.sizes, []int{batch, batch, batch, 1}) || !bytes.Equal(w.Body.Bytes(), data) || !w.Flushed {
		t.Fatalf("writes=%v flushed=%v", w.sizes, w.Flushed)
	}

	for i, deadline := range w.deadlines {
		if deadline.IsZero() {
			continue
		}

		if deadline.Before(before.Add(30*time.Second)) || i > 0 && deadline.Before(w.deadlines[i-1]) {
			t.Fatalf("deadline did not roll: %v", w.deadlines)
		}
	}

	if len(w.deadlines) != 15 || observed.Status != http.StatusPartialContent || observed.Bytes != int64(len(data)) || observed.Aborted || observed.Duration <= 0 {
		t.Fatalf("observation=%+v deadlines=%v", observed, w.deadlines)
	}
}

type racerShortWriter struct {
	*racerDeadlineWriter
}

func (w *racerShortWriter) Write(p []byte) (int, error) {
	if len(w.sizes) > 0 {
		return w.racerDeadlineWriter.Write(p[:7])
	}

	return w.racerDeadlineWriter.Write(p)
}

func TestRacerIOBatchedShortWrite(t *testing.T) {
	const batch = 256 * 1024

	w := &racerShortWriter{&racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
	wrapped := &racerResponseWriter{ResponseWriter: w, timeout: time.Second}

	n, err := wrapped.Write(make([]byte, 3*batch))
	if n != batch+7 || !errors.Is(err, io.ErrShortWrite) || !wrapped.failed || wrapped.bytes != int64(n) {
		t.Fatalf("bytes=%d err=%v failed=%v accounted=%d", n, err, wrapped.failed, wrapped.bytes)
	}

	if !slices.Equal(w.sizes, []int{batch, 7}) || len(w.deadlines) != 6 {
		t.Fatalf("writes=%v deadlines=%v", w.sizes, w.deadlines)
	}
}

func TestRacerIOFailuresObserved(t *testing.T) {
	for _, mode := range []string{"write", "deadline", "abort"} {
		t.Run(mode, func(t *testing.T) {
			w := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}

			failure := errors.New("failed")
			if mode == "write" {
				w.writeErr = failure
			}

			if mode == "deadline" {
				w.deadlineErr = failure
			}

			var observed RacerHTTPObservation

			handler := RacerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "abort" {
					panic(http.ErrAbortHandler)
				}

				_, err := w.Write([]byte("body"))
				if err != nil {
					panic(http.ErrAbortHandler)
				}
			}), time.Second, func(o RacerHTTPObservation) { observed = o })

			func() {
				defer func() {
					if recovered := recover(); recovered != http.ErrAbortHandler {
						t.Fatalf("panic = %v", recovered)
					}
				}()

				handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			}()

			if !observed.Aborted || observed.Bytes != 0 {
				t.Fatalf("observation = %+v", observed)
			}
		})
	}
}

func TestRacerIOStalledDownstream(t *testing.T) {
	done := make(chan error, 1)

	server := httptest.NewServer(RacerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := make([]byte, racerWriteChunk)
		for {
			if _, err := w.Write(data); err != nil {
				done <- err
				return
			}
		}
	}), 50*time.Millisecond, nil))
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
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
			t.Fatalf("expected write timeout, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled downstream was not interrupted")
	}
}

func TestRacerIOProgressBeyondTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond

	server := httptest.NewServer(RacerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 5 {
			// An upstream pause may exceed the write timeout. Each downstream
			// write still receives a fresh budget when bytes become available.
			time.Sleep(2 * timeout)

			if _, err := io.WriteString(w, "chunk"); err != nil {
				return
			}

			w.(http.Flusher).Flush()
		}
	}), timeout, nil))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "chunkchunkchunkchunkchunk" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestRacerIOCancellationDeadlineSurvivesRefresh(t *testing.T) {
	for _, operation := range []string{"Write", "ReadFrom", "Flush"} {
		t.Run(operation, func(t *testing.T) {
			dst := &racerFastWriter{racerDeadlineWriter: &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
			w := &racerResponseWriter{ResponseWriter: dst, timeout: time.Second}
			w.WriteHeader(http.StatusOK)
			controller := http.NewResponseController(w)
			// Reproduce the precise SDK interleaving: its initial deadline returns,
			// cancellation installs an immediate deadline, then the wrapper refreshes.
			if err := controller.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}

			interrupt := time.Now().Add(-time.Second)
			done := make(chan error, 1)

			go func() { done <- controller.SetWriteDeadline(interrupt) }()

			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// Even another external refresh must not clear the interruption.
			if err := controller.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}

			var err error

			switch operation {
			case "Write":
				_, err = w.Write([]byte("payload"))
			case "ReadFrom":
				_, err = w.ReadFrom(&io.LimitedReader{R: bytes.NewReader([]byte("payload")), N: 7})
			case "Flush":
				err = w.FlushError()
			}

			if !errors.Is(err, os.ErrDeadlineExceeded) || !w.failed || w.bytes != 0 || dst.reads != 0 || dst.Body.Len() != 0 || dst.Flushed {
				t.Fatalf("interruption lost: err=%v failed=%v bytes=%d reads=%d flushed=%v", err, w.failed, w.bytes, dst.reads, dst.Flushed)
			}

			if got := dst.deadlines[len(dst.deadlines)-1]; !got.Equal(interrupt) {
				t.Fatalf("deadline refreshed past interruption: %v; want %v", got, interrupt)
			}
			// SDK cleanup explicitly clears the latch. Rolling deadlines and the
			// handler's final flush must then work normally (failed stays recorded).
			if err := controller.SetWriteDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}

			if _, err := w.Write([]byte("next")); err != nil {
				t.Fatal(err)
			}

			if err := w.FlushError(); err != nil {
				t.Fatal(err)
			}

			if dst.Body.String() != "next" || !dst.Flushed || !dst.deadlines[len(dst.deadlines)-1].IsZero() {
				t.Fatal("rolling writes/final flush did not resume after clear")
			}
		})
	}
}

func TestRacerIOExternalDeadlineCapsRollingDeadline(t *testing.T) {
	dst := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	w := &racerResponseWriter{ResponseWriter: dst, timeout: time.Hour}

	deadline := time.Now().Add(time.Minute)
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}

	if err := w.FlushError(); err != nil {
		t.Fatal(err)
	}

	for _, got := range dst.deadlines {
		if !got.IsZero() && !got.Equal(deadline) {
			t.Fatalf("rolling deadline exceeded external bound: %v; want %v", got, deadline)
		}
	}
}

func TestRacerIOConcurrentDeadlineFailureAccounting(t *testing.T) {
	failure := errors.New("deadline failure")
	dst := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), deadlineErr: failure}
	w := &racerResponseWriter{ResponseWriter: dst, timeout: time.Second, status: http.StatusOK}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 1000 {
			// This callback path must not modify handler-owned accounting, even
			// when the underlying deadline operation fails.
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now()); !errors.Is(err, failure) {
				t.Errorf("external deadline error=%v", err)
			}
		}
	}()

	for range 1000 {
		_, err := w.Write([]byte("body"))
		if err == nil || !w.failed || w.status != http.StatusOK || w.bytes != 0 {
			t.Errorf("handler accounting: err=%v failed=%v status=%d bytes=%d", err, w.failed, w.status, w.bytes)
		}
	}

	<-done
}

type racerInterruptOnReturn struct {
	*racerDeadlineWriter
	interrupt func()
}

func (w racerInterruptOnReturn) Write(p []byte) (int, error) {
	w.interrupt()
	return w.ResponseRecorder.Write(p)
}

func (w racerInterruptOnReturn) ReadFrom(r io.Reader) (int64, error) {
	w.interrupt()
	return io.Copy(w.ResponseRecorder, r)
}

func (w racerInterruptOnReturn) FlushError() error {
	w.interrupt()
	w.Flush()

	return nil
}

func TestRacerIOOperationClearPreservesInterrupt(t *testing.T) {
	for _, operation := range []string{"Write", "ReadFrom", "Flush"} {
		t.Run(operation, func(t *testing.T) {
			dst := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			w := &racerResponseWriter{timeout: time.Second, status: http.StatusOK}
			interrupt := time.Now().Add(-time.Second)
			w.ResponseWriter = racerInterruptOnReturn{dst, func() {
				if err := http.NewResponseController(w).SetWriteDeadline(interrupt); err != nil {
					t.Error(err)
				}
			}}

			var err error

			switch operation {
			case "Write":
				_, err = w.Write([]byte("body"))
			case "ReadFrom":
				_, err = w.ReadFrom(&io.LimitedReader{R: bytes.NewReader([]byte("body")), N: 4})
			case "Flush":
				err = w.FlushError()
			}

			if err != nil {
				t.Fatal(err)
			}

			if !w.interrupted || !dst.deadlines[len(dst.deadlines)-1].Equal(interrupt) {
				t.Fatal("operation cleanup erased cancellation interrupt")
			}
		})
	}
}
