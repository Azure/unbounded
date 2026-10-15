// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"sync"
)

// BuildableClient exposes only transport copies, not its live pool. Track the
// connections opened by its final transport so shutdown closes the real pool.
type originConnectionPool struct {
	mu          sync.Mutex
	connections map[*originConnection]struct{}
	closed      bool
}

func (p *originConnectionPool) dial(next func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := next(ctx, network, address)
		if err != nil {
			return nil, err
		}

		p.mu.Lock()
		defer p.mu.Unlock()

		if p.closed {
			closeResource(conn)
			return nil, net.ErrClosed
		}

		if p.connections == nil {
			p.connections = make(map[*originConnection]struct{})
		}

		tracked := &originConnection{Conn: conn, pool: p}
		p.connections[tracked] = struct{}{}

		return tracked, nil
	}
}

func (p *originConnectionPool) close() {
	p.mu.Lock()
	p.closed = true
	connections := p.connections
	p.connections = nil
	p.mu.Unlock()

	for conn := range connections {
		closeResource(conn)
	}
}

type originConnection struct {
	net.Conn
	pool *originConnectionPool
}

func (c *originConnection) Close() error {
	c.pool.mu.Lock()
	delete(c.pool.connections, c)
	c.pool.mu.Unlock()

	return c.Conn.Close()
}
