// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"sync"
	"time"
)

const copyBufferSize = 32 * 1024

// Value is an immutable full-object or selected-range stream. One goroutine may
// consume it using Read or WriteTo (including io.Copy); Metadata and Close may be called
// concurrently. A Value must not be copied. Construct it with Client.Get.
type Value struct {
	stream    *PageStream
	ordered   *orderedRead
	mu        sync.Mutex
	client    *Client
	pool      *connectionPool
	ctx       context.Context
	cancel    context.CancelFunc
	stop      func() bool
	body      io.ReadCloser
	terminal  error
	finished  chan struct{}
	slot      bool
	metadata  Metadata
	request   OriginRequest
	remaining int64
	offset    int64
	end       int64
}

// Metadata returns the immutable total-size/tag/expiry snapshot, never a
// remaining length.
func (v *Value) Metadata() Metadata { return v.metadata }

func (v *Value) err() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.terminal
}

func closeBody(body io.Closer) {
	if body != nil {
		if err := body.Close(); err != nil {
			return
		}
	}
}

func (v *Value) finish(err error) {
	v.mu.Lock()
	if v.terminal != nil {
		done := v.finished
		v.mu.Unlock()

		if done != nil {
			<-done
		}

		return
	}

	if v.finished != nil {
		defer close(v.finished)
	}

	v.terminal = err
	body, slot, stop, ordered := v.body, v.slot, v.stop, v.ordered
	v.body, v.slot, v.stop = nil, false, nil
	v.request = OriginRequest{}
	v.mu.Unlock()

	if stop != nil {
		stop()
	}

	if v.cancel != nil {
		v.cancel()
	}

	closeBody(body)

	if ordered != nil {
		ordered.shutdown()
	}

	if v.client != nil {
		if slot {
			<-v.pool.slots
		}

		v.client.mu.Lock()
		delete(v.client.active, v)
		v.client.mu.Unlock()
	}
}

// Read consumes ordered page leases. An incomplete page is never exposed.
// A terminal error never restarts the version or opens another subscription.
func (v *Value) Read(p []byte) (int, error) {
	if v.stream != nil {
		return v.readPages(p)
	}

	if err := v.err(); err != nil {
		return 0, err
	}

	if v.ctx == nil {
		return 0, failure(ErrorClosed, "value", nil)
	}

	if err := v.ctx.Err(); err != nil {
		v.finish(ioFailure("read", err))
		return 0, v.err()
	}

	if len(p) == 0 {
		return 0, nil
	}

	if v.remaining == 0 {
		v.mu.Lock()
		body := v.body
		v.body = nil
		v.mu.Unlock()
		closeBody(body)

		v.finish(io.EOF)

		return 0, v.err()
	}

	v.mu.Lock()
	body, terminal := v.body, v.terminal
	v.mu.Unlock()

	if terminal != nil {
		return 0, terminal
	}

	if int64(len(p)) > v.remaining {
		p = p[:v.remaining]
	}

	n, err := body.Read(p)

	v.remaining -= int64(n)

	v.offset += int64(n)
	if err == io.EOF {
		if v.remaining != 0 {
			err = io.ErrUnexpectedEOF
		} else {
			err = nil
		}
	}

	if err != nil {
		if v.ctx.Err() != nil {
			err = v.ctx.Err()
		}

		v.finish(ioFailure("read", err))

		return n, v.err()
	}

	return n, nil
}

func (v *Value) readPages(p []byte) (int, error) {
	if err := v.err(); err != nil {
		return 0, err
	}

	if err := v.ctx.Err(); err != nil {
		return 0, v.stream.fail(ioFailure("read", err))
	}

	if len(p) == 0 {
		return 0, nil
	}

	n, err := v.ordered.read(p)
	v.offset += int64(n)

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

// WriteTo streams into w using 32 KiB scratch without invoking w.ReadFrom.
// The returned count includes only bytes accepted by w. The caller must still
// Close, including on writer failure. Copy buffers are bounded independently to
// MaxConnections for bulk and SmallObjectConnections for small objects per client,
// including canceled copies still blocked in caller-owned Write.
// If those blocked writers exhaust scratch admission, WriteTo returns
// ErrorUnavailable. Cancellation cannot interrupt an arbitrary destination Write.
func (v *Value) WriteTo(w io.Writer) (int64, error) {
	return v.writeTo(w)
}

func (v *Value) writeTo(w io.Writer) (int64, error) {
	if _, err := v.Read(nil); err != nil {
		if err == io.EOF {
			return 0, nil
		}

		return 0, err
	}

	c := v.client
	if sink, ok := w.(*FDSink); ok && v.stream != nil {
		done := make(chan struct{})
		stop := context.AfterFunc(v.ctx, func() {
			defer close(done)

			// A closed destination already interrupts any blocked write.
			if err := sink.connection.SetWriteDeadline(time.Now()); err != nil {
				return
			}
		})

		defer func() {
			if !stop() {
				<-done
			}

			// Deadline cleanup cannot recover a destination that has closed.
			if err := sink.connection.SetWriteDeadline(time.Time{}); err != nil {
				return
			}
		}()
	}

	slots, buffers := c.copySlots, c.copyBuffers
	if v.pool == &c.smallPool {
		slots, buffers = c.smallCopySlots, c.smallCopyBuffers
	}

	select {
	case slots <- struct{}{}:
	default:
		return 0, failure(ErrorUnavailable, "copy capacity", nil)
	}

	var buf *[copyBufferSize]byte
	select {
	case buf = <-buffers:
	default:
		buf = new([copyBufferSize]byte)
	}

	defer func() {
		c.mu.Lock()
		if !c.closed {
			buffers <- buf
		}
		c.mu.Unlock()
		<-slots
	}()

	var written int64

	empty := 0

	for {
		buffer := buf[:]

		n, readErr := v.Read(buffer)
		if n > 0 {
			empty = 0

			nw, writeErr := w.Write(buf[:n])
			if nw < 0 || nw > n {
				nw = 0

				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
			}

			written += int64(nw)

			if writeErr != nil {
				if _, ok := w.(*FDSink); ok && v.ctx.Err() != nil {
					return written, v.stream.fail(ioFailure("write", v.ctx.Err()))
				}

				return written, writeErr
			}

			if nw != n {
				return written, io.ErrShortWrite
			}
		} else if readErr == nil {
			empty++
			if empty == 100 {
				v.finish(ioFailure("copy", io.ErrNoProgress))
				return written, v.err()
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
