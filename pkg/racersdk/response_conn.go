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
	timer      *time.Timer
	generation uint64
	client     *Client
	once       sync.Once
}

func (conn *pooledConn) Close() error {
	var err error

	conn.once.Do(func() { err = conn.Conn.Close(); conn.client.stats.connections.Add(-1) })

	return err
}

func (c *Client) connection(ctx context.Context, pool *connectionPool, fresh bool) (*pooledConn, bool, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false, failure(ErrorClosed, "connection", nil)
	}

	if n := len(pool.idle); n != 0 && !fresh {
		conn := pool.idle[n-1]
		pool.idle = pool.idle[:n-1]

		conn.timer.Stop()
		conn.timer = nil
		conn.generation++
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

	return &pooledConn{Conn: conn, reader: bufio.NewReader(conn), client: c}, false, nil
}

func (c *Client) recycle(pool *connectionPool, conn *pooledConn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		closeBody(conn)
		return
	}

	pool.idle = append(pool.idle, conn)
	conn.generation++
	generation := conn.generation
	conn.timer = time.AfterFunc(c.config.IdleConnTimeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		if conn.generation != generation {
			return
		}

		for i, candidate := range pool.idle {
			if candidate == conn {
				pool.idle = append(pool.idle[:i], pool.idle[i+1:]...)

				closeBody(conn)

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

// responseBody owns one exchange. Close interrupts reads; only a fully consumed,
// validated frame is reusable. It never drains, retries, or parses body bytes.
type responseBody struct {
	mu        sync.Mutex
	client    *Client
	pool      *connectionPool
	conn      *pooledConn
	remaining int64
	reusable  bool
	closed    bool
}

func (b *responseBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, failure(ErrorClosed, "body", nil)
	}

	remaining := b.remaining
	b.mu.Unlock()

	if remaining == 0 {
		return 0, io.EOF
	}

	p = p[:min(int64(len(p)), remaining)]
	n, err := b.conn.reader.Read(p)
	b.client.stats.bytesRead.Add(uint64(n))
	b.mu.Lock()

	b.remaining -= int64(n)
	if err == io.EOF && b.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}

	if err != nil {
		b.reusable = false
	}
	b.mu.Unlock()

	return n, err
}

func (b *responseBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true
	if b.reusable && b.remaining == 0 && b.conn.reader.Buffered() == 0 {
		b.client.recycle(b.pool, b.conn)
		return nil
	}

	return b.conn.Close()
}

func (v *Value) open(r OriginRequest, snapshot *Metadata) (Metadata, int64, error) {
	head, err := requestHead(r)
	if err != nil {
		return Metadata{}, 0, err
	}

	for attempt := range 2 {
		result, started, reused, err := v.exchange(head, r, snapshot, attempt != 0)
		if err == nil {
			return result.metadata, result.length, nil
		}

		if attempt != 0 || !reused || started || v.ctx.Err() != nil || !staleConnectionError(err) {
			return Metadata{}, 0, err
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

func (v *Value) exchange(head []byte, r OriginRequest, snapshot *Metadata, fresh bool) (result wireResponse, started, reused bool, err error) {
	conn, reused, err := v.client.connection(v.ctx, v.pool, fresh)
	if err != nil {
		return result, false, reused, err
	}

	body := &responseBody{client: v.client, pool: v.pool, conn: conn, remaining: -1}
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

	result, err = parseResponseHead(head, r, snapshot)
	if err != nil {
		return result, true, reused, err
	}

	if v.pool == &v.client.smallPool && result.metadata.Size > PageSize {
		return result, true, reused, failure(ErrorInvalidArgument, "small object size", nil)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return result, true, reused, ioFailure("deadline", err)
	}

	body.mu.Lock()
	body.remaining = result.length
	body.reusable = !result.close
	body.mu.Unlock()

	return result, true, reused, nil
}
