// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

// preparedPage owns page attempts and their socket, including cancellation and
// framing state. A lookahead worker owns it until joined; adoption moves the
// owner itself, without changing its context or socket cleanup. Sequential pages
// can reuse the owner and its connection after consuming the previous response.
// Only attempt counters may be read concurrently with preparation.
type preparedPage struct {
	object        *Object
	ctx           context.Context
	cancel        context.CancelFunc
	conn          *streamConn
	stop          func() bool
	stopped       chan struct{}
	pageOffset    int64
	pageEnd       int64
	responseClose bool
	operation     string
	statusCode    int
	requests      atomic.Int64
	retries       atomic.Int64
}

func newPreparedPage(ctx context.Context, object *Object, offset int64) *preparedPage {
	ctx, cancel := context.WithCancel(ctx)

	return &preparedPage{object: object, ctx: ctx, cancel: cancel, pageOffset: offset}
}

// release retires only the socket, allowing a rejected attempt to retry under
// the same context. Stop and join cancellation before making a socket reusable.
func (p *preparedPage) release(reuse bool) {
	if p.conn == nil {
		return
	}

	if !p.stop() {
		<-p.stopped

		reuse = false
	}

	if reuse && p.ctx.Err() == nil {
		p.object.client.streamPool.put(p.conn)
	} else {
		_ = p.conn.Close() //nolint:errcheck // Preserve the transfer error.
	}

	p.conn = nil
}

// close ends ownership. Cancel only after detaching the socket callback so a
// successfully consumed connection can be reused by another stream safely.
func (p *preparedPage) close(reuse bool) {
	p.release(reuse)
	p.cancel()
}

func (p *preparedPage) preparePage(offset, end int64) error {
	p.operation = "page_connect"
	p.pageOffset = offset
	p.statusCode = 0

	if err := p.ctx.Err(); err != nil {
		return err
	}

	if p.conn == nil {
		c, err := p.object.client.streamPool.get(p.ctx)
		if err != nil {
			return err
		}

		p.conn = c
		p.stopped = make(chan struct{})
		done := p.stopped

		p.stop = context.AfterFunc(p.ctx, func() {
			_ = c.Close() //nolint:errcheck // Cancellation interrupts socket I/O.

			close(done)
		})
		if deadline, ok := p.ctx.Deadline(); ok {
			if err := c.SetDeadline(deadline); err != nil {
				return err
			}
		}
	}

	p.pageEnd = offset + min(PageSize-offset%PageSize, end-offset)

	r, err := p.object.client.request(p.ctx, http.MethodGet, p.object.target)
	if err != nil {
		return err
	}

	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, p.pageEnd-1))
	r.Header.Set("If-Match", p.object.meta.ETag)

	p.operation = "page_request"

	p.requests.Add(1)

	if err := r.Write(p.conn); err != nil {
		return err
	}

	p.operation = "page_headers"

	resp, err := p.conn.response(r)
	if err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}

		return err
	}

	p.statusCode = resp.StatusCode

	p.operation = "page_validate"

	if resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 504 {
		if err := identityResponse(resp); err != nil {
			return err
		}
	}

	if err := p.object.validatePage(resp, offset, p.pageEnd-1); err != nil {
		return err
	}

	p.responseClose = resp.Close
	p.operation = "page_body"

	return nil
}
