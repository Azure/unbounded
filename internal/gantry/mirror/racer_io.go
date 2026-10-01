// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"errors"
	"io"
	"net/http"
	"time"
)

const racerWriteChunk = 256 * 1024

// Socket transfers avoid scratch copying; amortize ReadFrom, deadline and pipe
// setup over a larger bounded batch without changing fallback write granularity.
const racerSocketChunk = 256 * 1024

// RacerHTTPObservation reports actual downstream bytes, final HTTP status, and
// handler duration. Aborted distinguishes incomplete responses after headers.
type RacerHTTPObservation struct {
	Method   string
	Status   int
	Bytes    int64
	Duration time.Duration
	Aborted  bool
}

// RacerHTTPHandler bounds each write and flush independently, so progressing
// responses have no overall deadline. Zero timeout selects 30 seconds. observe
// may be nil; otherwise it must be concurrency-safe and must not block.
func RacerHTTPHandler(next http.Handler, timeout time.Duration, observe func(RacerHTTPObservation)) http.Handler {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		writer := &racerResponseWriter{ResponseWriter: w, timeout: timeout}
		completed := false

		defer func() {
			if observe != nil {
				status := writer.status
				if status == 0 && completed {
					status = http.StatusOK
				}

				observe(RacerHTTPObservation{Method: r.Method, Status: status, Bytes: writer.bytes, Duration: time.Since(started), Aborted: !completed || writer.failed})
			}
		}()

		next.ServeHTTP(writer, r)
		// Flush the final buffered headers/body under a fresh deadline, including
		// HEAD and short responses. net/http's final flush must not be unbounded.
		if err := writer.FlushError(); err != nil {
			panic(http.ErrAbortHandler)
		}

		completed = true
	})
}

type racerResponseWriter struct {
	http.ResponseWriter
	timeout time.Duration
	status  int
	bytes   int64
	failed  bool
}

func (w *racerResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *racerResponseWriter) deadline() error {
	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil // Recorders and non-network writers have no socket to deadline.
	}

	if err != nil {
		w.failed = true
	}

	return err
}

func (w *racerResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}

	if err := w.deadline(); err != nil {
		panic(http.ErrAbortHandler)
	}

	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *racerResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	total := 0

	for len(p) > 0 {
		if err := w.deadline(); err != nil {
			return total, err
		}

		chunk := p[:min(len(p), racerWriteChunk)]
		n, err := w.ResponseWriter.Write(chunk)
		total += n

		w.bytes += int64(n)
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}

		if err != nil {
			w.failed = true
			return total, err
		}

		p = p[n:]
	}

	return total, nil
}

// ReadFrom keeps net/http in charge of framing and connection reuse. Only a
// bounded source supplied by the SDK can use the underlying fast path. Preserve
// the concrete socket under a single limiter so net.TCPConn can recognize it.
func (w *racerResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	source, bounded := r.(*io.LimitedReader)

	fast, supported := w.ResponseWriter.(io.ReaderFrom)
	if !bounded || !supported {
		return io.Copy(struct{ io.Writer }{w}, r)
	}

	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	var total int64

	for source.N > 0 {
		if err := w.deadline(); err != nil {
			return total, err
		}

		remaining := source.N
		source.N = min(remaining, int64(racerSocketChunk))
		chunk := source.N
		n, err := fast.ReadFrom(source)
		consumed := chunk - source.N
		source.N += remaining - chunk
		total += n

		w.bytes += n
		if err == nil && (n != chunk || consumed != chunk) {
			err = io.ErrUnexpectedEOF
		}

		if err != nil {
			w.failed = true
			return total, err
		}
	}

	return total, nil
}

func (w *racerResponseWriter) Flush() {
	if err := w.FlushError(); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (w *racerResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	if err := w.deadline(); err != nil {
		return err
	}

	err := http.NewResponseController(w.ResponseWriter).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}

	if err != nil {
		w.failed = true
	}

	return err
}
