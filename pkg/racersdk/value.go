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
// Read (including through io.Copy); Metadata and Close may be called
// concurrently. A Value must not be copied. Construct it with Client.Get.
type Value struct {
	mu        sync.Mutex
	client    *Client
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
	pending   []*pendingPage
	workers   sync.WaitGroup
	next      int64
}

type pendingPage struct {
	done   chan struct{}
	body   io.ReadCloser
	length int64
	err    error
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
	pending := v.pending
	v.pending = nil
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

	for _, page := range pending {
		closeBody(page.body)
	}

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
// consumed, it opens a bounded window of pinned pages across pooled connections.
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

		if v.offset == int64(v.metadata.Size) {
			v.finish(io.EOF)
			return 0, v.err()
		}

		if err := v.advance(); err != nil {
			v.finish(err)
			return 0, v.err()
		}
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

// Submit in stream order, reserving actual pool capacity before starting a
// worker. Speculative pages never wait for capacity and therefore cannot take
// every connection while the next required page is queued behind them.
func (v *Value) advance() error {
	v.mu.Lock()
	if v.next == 0 {
		v.next = v.offset
	}
	v.mu.Unlock()

	for {
		v.mu.Lock()
		if v.terminal != nil {
			err := v.terminal
			v.mu.Unlock()

			return err
		}

		count := len(v.pending)
		if count >= v.client.window || v.next == int64(v.metadata.Size) {
			v.mu.Unlock()
			break
		}
		v.mu.Unlock()

		if count == 0 {
			select {
			case v.client.pages <- struct{}{}:
			case <-v.ctx.Done():
				return ioFailure("continuation", v.ctx.Err())
			}
		} else {
			select {
			case v.client.pages <- struct{}{}:
			default:
				return v.takePage()
			}
		}

		v.mu.Lock()
		if v.terminal != nil {
			err := v.terminal
			v.mu.Unlock()
			<-v.client.pages

			return err
		}

		r := v.request
		r.operation, r.pin = OperationPinned, v.metadata.ETag
		length := min(int64(PageSize), int64(v.metadata.Size)-v.next)
		r.byteRange = Range{present: true, first: uint64(v.next), last: uint64(v.next + length - 1)}
		v.next += length
		page := &pendingPage{done: make(chan struct{})}
		v.pending = append(v.pending, page)
		v.workers.Add(1)
		v.mu.Unlock()

		go func() {
			defer v.workers.Done()

			_, page.length, page.body, page.err = v.fetch(r, &v.metadata)
			close(page.done)
		}()
	}

	return v.takePage()
}

func (v *Value) takePage() error {
	v.mu.Lock()
	if v.terminal != nil {
		err := v.terminal
		v.mu.Unlock()

		return err
	}

	page := v.pending[0]
	v.mu.Unlock()

	select {
	case <-page.done:
	case <-v.ctx.Done():
		return ioFailure("continuation", v.ctx.Err())
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.terminal != nil {
		return v.terminal
	}

	if page.err != nil {
		return page.err
	}

	v.pending[0] = nil
	v.pending = v.pending[1:]
	v.body, v.remaining = page.body, page.length

	return nil
}

// Close cancels in-flight reads/continuation and closes without draining. It is
// idempotent. Later consumption reports a typed closed error, except that an
// already observed clean EOF remains EOF.
func (v *Value) Close() error {
	v.finish(failure(ErrorClosed, "value", nil))
	return nil
}
