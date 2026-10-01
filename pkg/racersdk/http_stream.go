// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

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

	c := v.client

	slots, buffers := c.copySlots, c.copyBuffers
	if v.pool == &c.smallPool {
		slots, buffers = c.smallCopySlots, c.smallCopyBuffers
	}

	select {
	case slots <- struct{}{}:
	default:
		return 0, failure(ErrorUnavailable, "copy capacity", nil)
	}

	h := &streamingHTTP{value: v, writer: w, controller: http.NewResponseController(w)}
	select {
	case h.buffer = <-buffers:
	default:
		h.buffer = new([copyBufferSize]byte)
	}

	defer func() {
		c.mu.Lock()
		if !c.closed {
			buffers <- h.buffer
		}
		c.mu.Unlock()
		<-slots
	}()

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
		s.deliveredBytes += uint64(length)
		// Return even the final credit before waiting for Complete. A one-credit
		// peer is allowed to wait for this release before emitting Complete.
		if err := s.release(number, length); err != nil {
			return written, err
		}
	}

	var frame [21]byte
	if err := s.readFull(frame[:]); err != nil {
		return written, err
	}

	if frame[0] != 2 || binary.BigEndian.Uint64(frame[1:9]) != s.pages || binary.BigEndian.Uint64(frame[9:17]) != s.end-s.first || binary.BigEndian.Uint32(frame[17:]) != 0 || s.deliveredBytes != s.end-s.first {
		return written, failure(ErrorProtocol, "subscription complete", nil)
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
	var frame [21]byte
	if err := s.readFull(frame[:]); err != nil {
		return 0, 0, err
	}

	number := binary.BigEndian.Uint64(frame[1:9])
	offset := binary.BigEndian.Uint64(frame[9:17])
	length := binary.BigEndian.Uint32(frame[17:])
	expected := s.first/uint64(PageSize) + s.delivered
	start := max(s.first, expected*uint64(PageSize))

	end := min(s.end, (expected+1)*uint64(PageSize))
	if frame[0] != 1 || number != expected || offset != start || length == 0 || uint64(length) != end-start {
		return 0, 0, failure(ErrorProtocol, "subscription frame", nil)
	}

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
		buffered := s.conn.reader.Buffered()
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

		n, readErr := s.conn.reader.Read(h.buffer[:batch])
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
			if readErr == io.EOF {
				readErr = io.ErrUnexpectedEOF
			}

			return written, ioFailure("subscription payload", readErr)
		}

		if n == 0 {
			return written, ioFailure("subscription payload", io.ErrNoProgress)
		}
	}

	return written, nil
}
