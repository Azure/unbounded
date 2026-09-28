// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
)

type (
	pageResult struct {
		value *Value
		err   error
	}
	pageJob struct{ result chan pageResult }
)

// A batch borrows the parent's admitted slot for its first page. Additional pages
// take available bulk slots without waiting, so concurrent Values cannot deadlock
// while holding all slots. Each child owns its socket and cancellation callback.
func (v *Value) advance() (int64, error) {
	if v.client.config.PageWindow <= 1 || v.window || v.pool != &v.client.bulk {
		v.mu.Lock()
		r := v.request
		v.mu.Unlock()
		r.operation, r.pin = OperationPinned, v.metadata.ETag
		r.byteRange = Range{present: true, first: uint64(v.offset), last: uint64(v.end - 1)}
		_, length, err := v.open(r, &v.metadata)

		return length, err
	}

	v.mu.Lock()
	if v.terminal != nil {
		err := v.terminal
		v.mu.Unlock()

		return 0, err
	}

	if len(v.pending) == 0 {
	batch:
		for next, i := v.offset, 0; next < v.end && i < min(v.client.config.PageWindow, cap(v.pool.slots)); i++ {
			extra := i != 0
			if extra {
				select {
				case v.pool.slots <- struct{}{}:
				default:
					break batch
				}
			}

			end := min(v.end, next+int64(PageSize))
			ctx, cancel := context.WithCancel(v.ctx)
			child := &Value{client: v.client, pool: v.pool, ctx: ctx, cancel: cancel, slot: extra, finished: make(chan struct{}), window: true, metadata: v.metadata, offset: next, end: end}
			r := v.request
			r.operation, r.pin = OperationPinned, v.metadata.ETag
			r.byteRange = Range{present: true, first: uint64(next), last: uint64(end - 1)}
			job := &pageJob{result: make(chan pageResult, 1)}

			v.pending = append(v.pending, job)
			v.client.pages <- struct{}{}

			v.workers.Add(1)
			go func() {
				defer v.workers.Done()

				stopPending(child, context.AfterFunc(ctx, func() { child.finish(ioFailure("page", ctx.Err())) }))
				_, length, err := child.open(r, &v.metadata)

				child.remaining = length
				if err != nil {
					child.finish(err)
				}

				job.result <- pageResult{child, err}
			}()

			next = end
		}
	}

	job := v.pending[0]
	v.mu.Unlock()

	select {
	case result := <-job.result:
		v.mu.Lock()
		if v.terminal != nil {
			// Return ownership to terminal cleanup, which drains every queued job.
			job.result <- result

			err := v.terminal
			v.mu.Unlock()

			return 0, err
		}

		v.pending = v.pending[1:]
		v.body = &windowBody{Value: result.value, pages: v.client.pages}
		v.mu.Unlock()

		return result.value.remaining, result.err
	case <-v.ctx.Done():
		return 0, ioFailure("page", v.ctx.Err())
	}
}

type windowBody struct {
	*Value
	pages  chan struct{}
	closed bool
}

func (b *windowBody) Close() error {
	if !b.closed {
		b.closed = true
		<-b.pages
	}

	return b.Value.Close()
}

var _ io.ReadCloser = (*windowBody)(nil)
