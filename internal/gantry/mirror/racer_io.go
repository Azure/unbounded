// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
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
		// No upstream work remains. Keep final protocol bytes (HTTP/1 chunk
		// terminator or HTTP/2 END_STREAM) bounded when net/http finishes the
		// handler. net/http owns deadline cleanup at that lifecycle boundary.
		if err := writer.deadline(); err != nil {
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
	// Only deadline state is shared with the SDK cancellation callback. Status,
	// bytes, and failed remain owned by the handler goroutine.
	deadlineMu       sync.Mutex
	externalDeadline time.Time
	interrupted      bool
}

func (w *racerResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetWriteDeadline coordinates SDK deadlines with rolling handler deadlines.
// An immediate interruption is sticky until explicitly cleared. Serializing the
// underlying calls as well as the state prevents a refresh from racing past it.
func (w *racerResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()

	if !w.interrupted || deadline.IsZero() {
		w.externalDeadline = deadline
		w.interrupted = !deadline.IsZero() && !deadline.After(time.Now())
	}

	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.externalDeadline)
}

func (w *racerResponseWriter) deadline() error {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()

	now := time.Now()
	if w.interrupted || !w.externalDeadline.IsZero() && !w.externalDeadline.After(now) {
		w.failed = true
		return os.ErrDeadlineExceeded
	}

	deadline := now.Add(w.timeout)
	if !w.externalDeadline.IsZero() && w.externalDeadline.Before(deadline) {
		deadline = w.externalDeadline
	}

	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
	if errors.Is(err, http.ErrNotSupported) {
		return nil // Recorders and non-network writers have no socket to deadline.
	}

	if err != nil {
		w.failed = true
	}

	return err
}

// End a downstream operation without clearing an SDK cancellation interrupt.
// Keep the external bound as state for nested Write calls, but disarm its timer
// during upstream-only waits, including ReadFrom's userspace copy fallback.
func (w *racerResponseWriter) clearDeadline() error {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()

	if w.interrupted {
		return nil
	}

	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Time{})
	if errors.Is(err, http.ErrNotSupported) {
		return nil
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

	if err := w.clearDeadline(); err != nil {
		panic(http.ErrAbortHandler)
	}
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
		if clearErr := w.clearDeadline(); err == nil {
			err = clearErr
		}

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
		if err := w.clearDeadline(); err != nil {
			return 0, err
		}

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
		if clearErr := w.clearDeadline(); err == nil {
			err = clearErr
		}

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
	clearErr := w.clearDeadline()

	if errors.Is(err, http.ErrNotSupported) {
		err = nil
	}

	if err == nil {
		err = clearErr
	}

	if err != nil {
		w.failed = true
	}

	return err
}
