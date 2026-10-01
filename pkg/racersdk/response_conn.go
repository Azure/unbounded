// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

type connectionPool struct {
	slots  chan struct{}
	queued chan struct{}
	idle   []*pooledConn // protected by Client.mu, including timer ownership
}

type pooledConn struct {
	net.Conn
	reader     *bufio.Reader
	timer      connectionTimer
	expiresAt  time.Time // fixed at dial success; retains the monotonic clock
	generation uint64
	client     *Client
	once       sync.Once
}

type connectionTimer interface {
	Stop() bool
}

// Integer nanoseconds in [ceil(3*maxAge/4), maxAge], without multiplication
// overflow or a zero random bound, even for a one-nanosecond lifetime.
func jitteredConnAge(maxAge time.Duration, int64N func(int64) int64) time.Duration {
	spread := maxAge / 4

	return maxAge - spread + time.Duration(int64N(int64(spread)+1))
}

func (conn *pooledConn) Close() error {
	return conn.close(false)
}

func (conn *pooledConn) retire() {
	// Retirement releases accounting even if closing the socket reports an error.
	if err := conn.close(true); err != nil {
		return
	}
}

func (conn *pooledConn) close(rotation bool) error {
	var err error

	conn.once.Do(func() {
		err = conn.Conn.Close()
		conn.client.stats.connections.Add(-1)

		if rotation {
			conn.client.stats.connectionRotations.Add(1)
		}
	})

	return err
}

func (c *Client) connection(ctx context.Context, pool *connectionPool, fresh bool) (*pooledConn, bool, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false, failure(ErrorClosed, "connection", nil)
	}

	for !fresh && len(pool.idle) != 0 {
		n := len(pool.idle)
		conn := pool.idle[n-1]
		pool.idle = pool.idle[:n-1]

		conn.timer.Stop()
		conn.timer = nil

		conn.generation++
		if !c.connNow().Before(conn.expiresAt) {
			conn.retire()
			continue
		}

		c.mu.Unlock()
		c.stats.connectionReuses.Add(1)

		return conn, true, nil
	}
	c.mu.Unlock()
	c.stats.dials.Add(1)

	conn, err := c.dial(ctx, "unix", c.path)
	if err != nil {
		return nil, false, ioFailure("dial", err)
	}

	if err := ctx.Err(); err != nil {
		closeBody(conn)
		return nil, false, ioFailure("dial", err)
	}

	c.stats.connections.Add(1)

	return &pooledConn{Conn: conn, reader: bufio.NewReader(conn), client: c, expiresAt: c.connNow().Add(c.connAge())}, false, nil
}

func (c *Client) recycle(pool *connectionPool, conn *pooledConn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		closeBody(conn)
		return
	}

	remaining := conn.expiresAt.Sub(c.connNow())
	if remaining <= 0 {
		conn.retire()
		return
	}

	pool.idle = append(pool.idle, conn)
	conn.generation++
	generation := conn.generation
	conn.timer = c.connAfterFunc(min(c.config.IdleConnTimeout, remaining), func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		if conn.generation != generation {
			return
		}

		for i, candidate := range pool.idle {
			if candidate == conn {
				pool.idle = append(pool.idle[:i], pool.idle[i+1:]...)

				if !c.connNow().Before(conn.expiresAt) {
					conn.retire()
				} else {
					closeBody(conn)
				}

				return
			}
		}
	})
}

func (c *Client) closeIdleConnections() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, pool := range []*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool} {
		for _, conn := range pool.idle {
			conn.timer.Stop()
			closeBody(conn)
		}

		pool.idle = nil
	}
}

// responseBody owns a connection lease. Close interrupts subscription reads;
// only validated, bodyless HEAD responses can return to the idle pool.
type responseBody struct {
	mu       sync.Mutex
	client   *Client
	pool     *connectionPool
	conn     *pooledConn
	reusable bool
	closed   bool
}

func (b *responseBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true
	if b.reusable && b.conn.reader.Buffered() == 0 {
		b.client.recycle(b.pool, b.conn)
		return nil
	}

	return b.conn.Close()
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

		if attempt != 0 || !reused || started || v.ctx.Err() != nil || !staleConnectionError(err) {
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

// Timeouts are not stale-connection evidence. Retry only EOF/reset/broken-pipe,
// once on a fresh connection, and only if no response byte has been received.
func staleConnectionError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func (v *Value) exchangeHead(head []byte, r OriginRequest, fresh bool) (result wireResponse, started, reused bool, err error) {
	conn, reused, err := v.client.connection(v.ctx, v.pool, fresh)
	if err != nil {
		return result, false, reused, err
	}

	body := &responseBody{client: v.client, pool: v.pool, conn: conn}
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
		head, err = readHeadBytes(conn.reader, true)
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

	body.mu.Lock()
	body.reusable = !result.close
	body.mu.Unlock()

	return result, true, reused, nil
}
