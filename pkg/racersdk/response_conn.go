// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/connpool"
)

func (c *Client) connection(ctx context.Context, pool *connectionPool, fresh bool) (*connpool.Conn, bool, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return nil, false, failure(ErrorClosed, "connection", nil)
	}

	conn, reused, err := pool.Get(ctx, fresh)
	if errors.Is(err, connpool.ErrClosed) {
		return nil, false, failure(ErrorClosed, "connection", nil)
	}

	if err != nil {
		return nil, false, ioFailure("dial", err)
	}

	return conn, reused, nil
}

func (v *Value) openHead(r OriginRequest) (Metadata, error) {
	head, err := clientHead(r)
	if err != nil {
		return Metadata{}, err
	}

	for attempt := range 2 {
		result, started, reused, err := v.exchangeHead(head, r, attempt != 0)
		if err == nil {
			return result.metadata, nil
		}

		if attempt != 0 || !reused || started || v.ctx.Err() != nil || !connpool.StaleError(err) {
			return Metadata{}, err
		}

		v.mu.Lock()
		body := v.body
		v.body = nil
		v.mu.Unlock()
		closeBody(body)
		v.client.stats.retries.Add(1)
	}

	panic("unreachable exchange retry")
}

func (v *Value) exchangeHead(head []byte, r OriginRequest, fresh bool) (result wireResponse, started, reused bool, err error) {
	conn, reused, err := v.client.connection(v.ctx, v.pool, fresh)
	if err != nil {
		return result, false, reused, err
	}

	body := connpool.NewBody(conn)

	v.mu.Lock()
	if v.terminal != nil {
		err := v.terminal
		v.mu.Unlock()
		closeBody(body)

		return result, false, reused, err
	}

	v.body = body
	v.mu.Unlock()
	// Bound blocked request writes as well as response heads, but remove all
	// deadlines before exposing a body. Context cancellation closes the lease.
	if err = conn.SetWriteDeadline(time.Now().Add(v.client.config.ResponseHeaderTimeout)); err == nil {
		var n int

		n, err = conn.Write(head)
		if err == nil && n != len(head) {
			err = io.ErrShortWrite
		}
	}

	if err == nil {
		err = conn.SetReadDeadline(time.Now().Add(v.client.config.ResponseHeaderTimeout))
	}

	if err == nil {
		head, err = readHeadBytes(conn.Reader, true)
		started = len(head) != 0
	}

	if err != nil {
		if v.ctx.Err() != nil {
			err = v.ctx.Err()
		}

		var typed *Error
		if errors.As(err, &typed) {
			return result, started, reused, err
		}

		return result, started, reused, ioFailure("exchange", err)
	}

	result, err = parseResponseHead(head, r, nil)
	if err != nil {
		return result, true, reused, err
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return result, true, reused, ioFailure("deadline", err)
	}

	body.SetReusable(!result.close)

	return result, true, reused, nil
}
