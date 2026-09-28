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
		length := 3*racerWriteChunk + 1

		advertised := length
		if truncated {
			advertised++
		}

		source := &io.LimitedReader{R: bytes.NewReader(make([]byte, length)), N: int64(advertised)}

		n, err := wrapped.ReadFrom(source)
		if n != int64(length) || truncated != errors.Is(err, io.ErrUnexpectedEOF) || wrapped.failed != truncated {
			t.Fatalf("bytes=%d err=%v failed=%v", n, err, wrapped.failed)
		}

		if w.reads != 4 || len(w.deadlines) != 5 || wrapped.bytes != n || source.N != int64(advertised-length) {
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
	w := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}

	var observed RacerHTTPObservation

	handler := RacerHTTPHandler(http.HandlerFunc(func(wrapped http.ResponseWriter, _ *http.Request) {
		if !errors.Is(http.NewResponseController(wrapped).SetReadDeadline(time.Now()), http.ErrNotSupported) {
			t.Fatal("unexpected controller behavior")
		}

		if unwrapped := wrapped.(interface{ Unwrap() http.ResponseWriter }).Unwrap(); unwrapped != w {
			t.Fatal("wrong underlying writer")
		}

		wrapped.WriteHeader(http.StatusPartialContent)

		if _, err := io.Copy(wrapped, bytes.NewReader(make([]byte, 3*racerWriteChunk+1))); err != nil {
			t.Fatal(err)
		}

		wrapped.(http.Flusher).Flush()
	}), 0, func(o RacerHTTPObservation) { observed = o })
	before := time.Now()

	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(w.sizes) != 4 || w.sizes[3] != 1 || !w.Flushed {
		t.Fatalf("writes=%v flushed=%v", w.sizes, w.Flushed)
	}

	for i, deadline := range w.deadlines {
		if deadline.Before(before.Add(30*time.Second)) || i > 0 && deadline.Before(w.deadlines[i-1]) {
			t.Fatalf("deadline did not roll: %v", w.deadlines)
		}
	}

	if len(w.deadlines) < 7 || observed.Status != http.StatusPartialContent || observed.Bytes != 3*racerWriteChunk+1 || observed.Aborted || observed.Duration <= 0 {
		t.Fatalf("observation=%+v deadlines=%v", observed, w.deadlines)
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
