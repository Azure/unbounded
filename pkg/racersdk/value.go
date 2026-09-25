// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"sync"
)

const copyBufferSize = 32 * 1024

// Value is a full immutable object stream. One goroutine may consume it using
// Read or WriteTo (including sequential mixing); Metadata and Close may be called
// concurrently. A Value must not be copied. Construct it with Client.Get.
type Value struct {
	mu        sync.Mutex
	client    *Client
	ctx       context.Context
	cancel    context.CancelFunc
	stop      func() bool
	body      io.ReadCloser
	terminal  error
	slot      bool
	metadata  Metadata
	request   OriginRequest
	remaining int64
	continued bool
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
		v.mu.Unlock()
		return
	}

	v.terminal = err
	body, slot, stop := v.body, v.slot, v.stop
	v.body, v.slot, v.stop = nil, false, nil
	v.mu.Unlock()

	if stop != nil {
		stop()
	}

	if v.cancel != nil {
		v.cancel()
	}

	closeBody(body)

	if v.client != nil {
		if slot {
			<-v.client.slots
		}

		v.client.mu.Lock()
		delete(v.client.active, v)
		v.client.mu.Unlock()
	}
}

// Read copies directly from the current HTTP body into p. Once bootstrap is
// consumed, it lazily opens one pinned range covering all remaining pages.
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

		if v.continued || v.metadata.Size <= PageSize {
			v.finish(io.EOF)
			return 0, v.err()
		}

		v.continued = true
		r := v.request
		r.operation, r.pin = OperationPinned, v.metadata.ETag
		r.byteRange = Range{kind: RangeClosed, first: uint64(PageSize), last: uint64(v.metadata.Size) - 1}

		_, length, err := v.open(r, &v.metadata)
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

// WriteTo streams through Read with one 32 KiB scratch buffer. It honors partial
// destination writes and returns io.ErrShortWrite for a short write without an
// error. Destination failures terminate the Value and release its resources.
func (v *Value) WriteTo(w io.Writer) (int64, error) {
	if w == nil {
		err := failure(ErrorInvalidArgument, "write destination", nil)
		v.finish(err)

		return 0, err
	}

	buf := make([]byte, copyBufferSize)

	var total int64

	for {
		n, err := v.Read(buf)
		if n != 0 {
			written, writeErr := w.Write(buf[:n])
			if written < 0 || written > n {
				written, writeErr = 0, io.ErrShortWrite
			}

			total += int64(written)
			if writeErr == nil && written != n {
				writeErr = io.ErrShortWrite
			}

			if writeErr != nil {
				v.finish(ioFailure("write", writeErr))
				return total, ioFailure("write", writeErr)
			}
		}

		if err == io.EOF {
			return total, nil
		}

		if err != nil {
			return total, err
		}
	}
}

// Close cancels in-flight reads/continuation and closes without draining. It is
// idempotent. Later consumption reports a typed closed error, except that an
// already observed clean EOF remains EOF.
func (v *Value) Close() error {
	v.finish(failure(ErrorClosed, "value", nil))
	return nil
}
