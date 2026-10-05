// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

const copyBufferSize = 256 * 1024

// Value is an immutable full-object or selected-range stream. One goroutine may
// consume it using Read or WriteTo (including io.Copy); Metadata and Close may be called
// concurrently. A Value must not be copied. Construct it with Client.Get, or
// Client.GetStreaming for exclusive WriteToHTTP consumption.
type Value struct {
	*admissionLease
	stream    *PageStream
	streaming bool
	ordered   *orderedRead
	metadata  Metadata
}

// Metadata returns the immutable total-size/tag/expiry snapshot, never a
// remaining length.
func (v *Value) Metadata() Metadata { return v.metadata }

// Read consumes ordered page leases. An incomplete page is never exposed.
// A terminal error never restarts the version or opens another subscription.
// GetStreaming Values reject Read, including zero-length reads.
func (v *Value) Read(p []byte) (int, error) {
	if v.streaming {
		return 0, failure(ErrorInvalidArgument, "streaming value requires WriteToHTTP", nil)
	}

	if err := v.err(); err != nil {
		// Cancellation publishes the error before joining the receiver. A
		// consumer observing it must also wait for admission/storage cleanup.
		v.finish(err)
		return 0, err
	}

	if v.stream == nil {
		return 0, failure(ErrorClosed, "value", nil)
	}

	if err := v.ctx.Err(); err != nil {
		return 0, v.stream.fail(ioFailure("read", err))
	}

	if len(p) == 0 {
		return 0, nil
	}

	n, err := v.ordered.read(p)
	if err != nil {
		if err == io.EOF {
			v.finish(io.EOF)
		} else {
			err = v.stream.fail(err)
		}
	}

	return n, err
}

// Close cancels in-flight subscription reads and closes without draining. It is
// idempotent. Later consumption reports a typed closed error, except that an
// already observed clean EOF remains EOF.
func (v *Value) Close() error {
	v.finish(failure(ErrorClosed, "value", nil))
	return nil
}

// WriteTo streams into w using 256 KiB scratch without invoking w.ReadFrom.
// The returned count includes only bytes accepted by w. The caller must still
// Close, including on writer failure. Copy buffers are bounded independently to
// MaxConnections for bulk and SmallObjectConnections for small objects per client,
// including canceled copies still blocked in caller-owned Write.
// If those blocked writers exhaust scratch admission, WriteTo returns
// ErrorUnavailable. Cancellation cannot interrupt an arbitrary destination Write.
// GetStreaming Values reject WriteTo without consuming the subscription.
func (v *Value) WriteTo(w io.Writer) (int64, error) {
	if _, err := v.Read(nil); err != nil {
		if err == io.EOF {
			return 0, nil
		}

		return 0, err
	}

	buf, release, err := v.client.admitCopy(v.pool)
	if err != nil {
		return 0, err
	}
	defer release()

	var written int64

	for {
		buffer := buf[:]

		n, readErr := v.Read(buffer)
		if n > 0 {
			nw, writeErr := w.Write(buf[:n])
			if nw < 0 || nw > n {
				nw = 0

				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
			}

			written += int64(nw)

			if writeErr != nil {
				return written, writeErr
			}

			if nw != n {
				return written, io.ErrShortWrite
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return written, nil
			}

			return written, readErr
		}
	}
}

// orderedRead owns at most two page-sized buffers, including the current lease,
// queued leases and an in-flight receive. Negotiated credits can reduce this to
// one. No buffer crosses request boundaries. Only Release makes storage reusable.
// The connection admission slot stays held until consumption ends or cancellation
// has joined the receiver and dropped its buffers, not merely until wire EOF.
type orderedRead struct {
	mu     sync.Mutex
	stream *PageStream
	ready  chan *PageLease
	slots  chan struct{}
	done   chan struct{}
	err    error // Published by closing ready; read only after ready is drained.
	lease  *PageLease
	offset int
}

func (v *Value) startOrdered(s *PageStream) {
	limit := min(2, s.pageCredits, int(s.byteCredits/uint64(PageSize)))
	s.buffers = make(chan []byte, limit)
	r := &orderedRead{stream: s, ready: make(chan *PageLease, limit), slots: make(chan struct{}, limit), done: make(chan struct{})}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.terminal != nil {
		return
	}

	v.ordered = r

	v.cleanup = r.shutdown
	go r.receive()
}

func (r *orderedRead) receive() {
	defer close(r.done)
	defer close(r.ready)

	for {
		select {
		case r.slots <- struct{}{}:
		case <-r.stream.owner.ctx.Done():
			r.err = ioFailure("subscription", r.stream.owner.ctx.Err())
			return
		}

		p, err := r.stream.next()
		if err != nil {
			<-r.slots
			r.err = err

			return
		}
		// ready has room for every occupied slot, even if cancellation wins.
		r.ready <- p

		r.stream.mu.Lock()
		complete := r.stream.complete
		r.stream.mu.Unlock()

		if complete {
			r.err = io.EOF
			return
		}
	}
}

// shutdown runs after cancel and socket close. It never waits on caller-owned
// Write, which only sees the separately bounded WriteTo scratch buffer.
func (r *orderedRead) shutdown() {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.lease != nil {
		_ = r.lease.Release() //nolint:errcheck // Terminal cleanup drops local ownership; the closed socket needs no credit return.
		r.lease = nil
		<-r.slots
	}

	for p := range r.ready {
		_ = p.Release() //nolint:errcheck // Preserve the already published terminal error during cleanup.

		<-r.slots
	}

	for len(r.stream.buffers) > 0 {
		<-r.stream.buffers
	}
}

func (r *orderedRead) read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.stream.owner.err(); err != nil {
		return 0, err
	}

	if r.lease == nil {
		select {
		case lease, ok := <-r.ready:
			if !ok {
				return 0, r.err
			}

			r.lease, r.offset = lease, 0
		case <-r.stream.owner.ctx.Done():
			return 0, ioFailure("read", r.stream.owner.ctx.Err())
		}
	}

	n := copy(p, r.lease.Data[r.offset:])

	r.offset += n
	if r.offset == len(r.lease.Data) {
		err := r.lease.Release()
		r.lease = nil
		<-r.slots

		return n, err
	}

	return n, nil
}

// WriteToHTTP delivers a Value through the server's normal response lifecycle.
// The caller sets headers, including the selected range's Content-Length, and
// owns Close. Get Values retain buffered page validation. GetStreaming Values
// validate frame headers and forward bounded payload slices, using the writer's
// ReadFrom when a raw Unix socket is available. The final byte is withheld until
// Complete is validated; an incomplete page prefix can be exposed on failure.
// No connection is hijacked. Empty ranges validate Complete before returning,
// but cannot withhold a byte; callers must avoid committing empty responses early.
// Destination deadlines are used where supported for streaming transfers.
// Arbitrary blocked writers cannot be interrupted, but retain a separately
// bounded copy admission slot.
// A streaming transfer failure is terminal; a handler that has committed headers
// must abort its response rather than emit an error body or report success.
func (v *Value) WriteToHTTP(w http.ResponseWriter) (int64, error) {
	if v.streaming {
		return v.writeStreamingHTTP(w)
	}

	return v.WriteTo(w)
}

// streamingHTTP owns receive framing exclusively. It never creates page buffers
// or a background receiver. Scratch admission remains held through destination
// calls even after cancellation releases the connection admission slot.
type streamingHTTP struct {
	value      *Value
	writer     http.ResponseWriter
	buffer     *[copyBufferSize]byte
	controller *http.ResponseController
	deadlineMu sync.Mutex
}

func (v *Value) writeStreamingHTTP(w http.ResponseWriter) (written int64, result error) {
	if w == nil {
		return 0, failure(ErrorInvalidArgument, "HTTP destination", nil)
	}

	if err := v.err(); err != nil {
		v.finish(err)

		if err == io.EOF {
			return 0, nil
		}

		return 0, err
	}

	buffer, release, err := v.client.admitCopy(v.pool)
	if err != nil {
		return 0, err
	}
	defer release()

	h := &streamingHTTP{value: v, writer: w, buffer: buffer, controller: http.NewResponseController(w)}
	done := make(chan struct{})
	stop := context.AfterFunc(v.ctx, func() {
		defer close(done)

		h.deadlineMu.Lock()
		defer h.deadlineMu.Unlock()

		_ = h.controller.SetWriteDeadline(time.Now()) //nolint:errcheck // Unsupported arbitrary writers remain bounded by copy admission.
	})
	stopCancellation := sync.OnceFunc(func() {
		if !stop() {
			<-done
		}
	})

	defer func() {
		stopCancellation()

		_ = h.controller.SetWriteDeadline(time.Time{}) //nolint:errcheck // Best-effort cleanup after destination closure.
	}()

	written, result = h.transfer()
	if result != nil {
		return written, v.stream.fail(result)
	}
	// Successful finish cancels the Value context for resource cleanup. Disarm
	// and join destination cancellation first: an expired HTTP/2 deadline resets
	// the stream irreversibly, even if cleanup immediately clears that deadline.
	// The deferred call also joins the callback on failures, exactly once.
	stopCancellation()

	if err := v.ctx.Err(); err != nil {
		return written, v.stream.fail(ioFailure("HTTP transfer", err))
	}

	v.finish(io.EOF)

	if err := v.err(); err != io.EOF {
		return written, err
	}

	return written, nil
}

func (h *streamingHTTP) writeDeadline() error {
	h.deadlineMu.Lock()
	defer h.deadlineMu.Unlock()

	if err := h.value.ctx.Err(); err != nil {
		return ioFailure("HTTP write", err)
	}

	err := h.controller.SetWriteDeadline(time.Now().Add(h.value.client.config.BodyReadTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}

	return err
}

func (h *streamingHTTP) write(p []byte) (int64, error) {
	if err := h.writeDeadline(); err != nil {
		return 0, err
	}

	n, err := h.writer.Write(p)
	if clearErr := h.clearWriteDeadline(); err == nil {
		err = clearErr
	}

	if n < 0 || n > len(p) {
		n = 0

		if err == nil {
			err = io.ErrShortWrite
		}
	}

	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}

	return int64(n), err
}

// Clear only an ordinary operation deadline. Cancellation uses the same lock,
// so cleanup cannot erase its immediate interrupt, even if the callback has
// already run. Final cleanup may clear unconditionally after joining it.
func (h *streamingHTTP) clearWriteDeadline() error {
	h.deadlineMu.Lock()
	defer h.deadlineMu.Unlock()

	if err := h.value.ctx.Err(); err != nil {
		return ioFailure("HTTP write", err)
	}

	err := h.controller.SetWriteDeadline(time.Time{})
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}

	return err
}

func (h *streamingHTTP) transfer() (int64, error) {
	s := h.value.stream

	var (
		written int64
		final   [1]byte
	)

	for s.delivered < s.pages {
		number, length, err := s.streamingFrame()
		if err != nil {
			return written, err
		}

		last := s.delivered+1 == s.pages

		remaining := int64(length)
		if last {
			remaining--
		}

		n, err := h.payload(remaining)

		written += n
		if err != nil {
			return written, err
		}

		if last {
			if err := s.readPayload(final[:]); err != nil {
				return written, err
			}
		}

		s.delivered++
		// Return even the final credit before waiting for Complete. A one-credit
		// peer is allowed to wait for this release before emitting Complete.
		if err := s.release(number, length); err != nil {
			return written, err
		}
	}

	var frame [wire.FrameSize]byte
	if err := s.readFull(frame[:]); err != nil {
		return written, err
	}

	if err := s.validateFrame(wire.DecodeFrame(frame), "subscription complete"); err != nil {
		return written, err
	}

	s.mu.Lock()
	s.complete = true
	s.mu.Unlock()

	if s.end != s.first {
		n, err := h.write(final[:])
		return written + n, err
	}

	return written, nil
}

// streamingFrame reserves exactly one ordered page using negotiated credits.
// Payload is not hashed or buffered for whole-page validation in this mode.
func (s *PageStream) streamingFrame() (uint64, uint32, error) {
	var frame [wire.FrameSize]byte
	if err := s.readFull(frame[:]); err != nil {
		return 0, 0, err
	}

	f := wire.DecodeFrame(frame)
	if err := s.validateFrame(f, "subscription frame"); err != nil {
		return 0, 0, err
	}

	number, length := f.Number, f.Length

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.outstanding) >= s.pageCredits || uint64(length) > s.byteCredits-s.bytesHeld {
		return 0, 0, failure(ErrorProtocol, "subscription credits", nil)
	}

	s.outstanding[number] = length
	s.bytesHeld += uint64(length)

	return number, length, nil
}

func (h *streamingHTTP) payload(remaining int64) (int64, error) {
	s := h.value.stream

	var written int64

	for remaining > 0 {
		batch := min(remaining, int64(copyBufferSize))
		// Never bypass read-ahead: it may contain payload and later frame headers.
		buffered := s.conn.Reader.Buffered()
		raw, unix := s.conn.Conn.(*net.UnixConn)

		rf, fast := h.writer.(io.ReaderFrom)
		if buffered == 0 && unix && fast {
			if err := s.conn.SetReadDeadline(time.Now().Add(h.value.client.config.BodyReadTimeout)); err != nil {
				return written, ioFailure("subscription deadline", err)
			}

			if err := h.writeDeadline(); err != nil {
				return written, err
			}

			limited := &io.LimitedReader{R: raw, N: batch}

			n, err := rf.ReadFrom(limited)
			if clearErr := h.clearWriteDeadline(); err == nil {
				err = clearErr
			}

			_ = s.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve successful reads after peer closure.
			// With stdlib splice, LimitedReader.N advances by bytes delivered.
			// A destination failure can leave additional source bytes in the
			// kernel pipe. No public syscall accounting API exposes that loss,
			// so BytesRead may undercount source consumption on this error path.
			consumed := batch - limited.N
			if consumed >= 0 && consumed <= batch {
				h.value.client.stats.bytesRead.Add(uint64(consumed))
			}

			if n < 0 || n > batch {
				n = 0

				if err == nil {
					err = io.ErrShortWrite
				}
			}

			written += n

			if err == io.EOF || err == nil && limited.N != 0 {
				err = ioFailure("subscription payload", io.ErrUnexpectedEOF)
			}

			if err == nil && n != batch {
				err = io.ErrShortWrite
			}

			if err != nil {
				return written, err
			}

			remaining -= batch

			continue
		}

		if buffered > 0 {
			batch = min(batch, int64(buffered))
		}
		// Read once rather than ReadFull, so incomplete page prefixes have the
		// same streaming semantics with and without ReaderFrom.
		if err := s.conn.SetReadDeadline(time.Now().Add(h.value.client.config.BodyReadTimeout)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return written, ioFailure("subscription deadline", err)
		}

		n, readErr := s.conn.Reader.Read(h.buffer[:batch])
		_ = s.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve successful bytes after peer closure.

		h.value.client.stats.bytesRead.Add(uint64(n))

		if n > 0 {
			nw, err := h.write(h.buffer[:n])

			written += nw
			if err != nil {
				return written, err
			}

			remaining -= int64(n)
		}

		if readErr != nil {
			return written, ioFailure("subscription payload", truncation(readErr))
		}

		if n == 0 {
			return written, ioFailure("subscription payload", io.ErrNoProgress)
		}
	}

	return written, nil
}
