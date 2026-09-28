// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"sync"
)

const copyBufferSize = 32 * 1024

// Value is an immutable full-object or selected-range stream. One goroutine may
// consume it using Read or WriteTo (including io.Copy); Metadata and Close may be called
// concurrently. A Value must not be copied. Construct it with Client.Get.
type Value struct {
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
	pending   []*pageJob
	workers   sync.WaitGroup
	window    bool
}

// Metadata returns the initial total-size/tag/expiry snapshot, never a remaining
// length. A continuation's refreshed expiry does not change this value.
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
	body, slot, stop := v.body, v.slot, v.stop
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
	v.workers.Wait()
	v.mu.Lock()
	pending := v.pending
	v.pending = nil
	v.mu.Unlock()

	for _, job := range pending {
		closeBody((<-job.result).value)
		<-v.client.pages
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

// Read copies directly from the current HTTP body into p. Once bootstrap is
// consumed, it lazily opens one pinned range through the selected end.
// A terminal error preserves partial byte counts and never restarts the version.
func (v *Value) Read(p []byte) (int, error) {
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

		if v.offset == v.end {
			v.finish(io.EOF)
			return 0, v.err()
		}

		length, err := v.advance()
		if err != nil {
			v.finish(err)
			return 0, v.err()
		}

		v.remaining = length
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

// Close cancels in-flight reads/continuation and closes without draining. It is
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
	return v.writeTo(w, false)
}

func (v *Value) writeTo(w io.Writer, httpTransfer bool) (int64, error) {
	if _, err := v.Read(nil); err != nil {
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

		if httpTransfer && v.remaining > 0 {
			n, used, err := v.writeHTTPBody(w)

			written += n
			if err != nil {
				return written, err
			}

			if used {
				continue
			}
		}

		if sink, ok := w.(*FDSink); ok && v.remaining > 0 {
			n, used, err := v.spliceTo(sink)

			written += n
			if err != nil {
				return written, err
			}

			if used {
				continue
			}

			v.mu.Lock()
			body, ok := v.body.(*responseBody)
			v.mu.Unlock()

			if ok && body.conn.reader.Buffered() > 0 {
				buffer = buffer[:min(len(buffer), body.conn.reader.Buffered())]
			}
		}

		if httpTransfer {
			v.mu.Lock()
			body, ok := v.body.(*responseBody)
			v.mu.Unlock()

			if ok && body.conn.reader.Buffered() > 0 {
				buffer = buffer[:min(len(buffer), body.conn.reader.Buffered())]
			}
		}

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
