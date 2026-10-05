// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

// transportListener owns sockets from acceptance until close, including sockets
// not yet visible to net/http. Excess sockets are closed in the accept loop,
// without spawning a goroutine or queuing work. The kernel backlog is separate.
type transportListener struct {
	net.Listener
	config      *tls.Config
	timeout     time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	ready       chan net.Conn
	acceptDone  chan struct{}
	acceptErr   error
	connections chan struct{}
	handshakes  chan struct{}
	mu          sync.Mutex
	closed      bool
	live        map[*transportConn]struct{}
	closeOnce   sync.Once
	closeErr    error
}

func newTransportListener(ctx context.Context, listener net.Listener, config *tls.Config, limits Limits) *transportListener {
	ctx, cancel := context.WithCancel(ctx)

	l := &transportListener{
		Listener: listener, config: config, timeout: limits.WriteTimeout,
		ctx: ctx, cancel: cancel, ready: make(chan net.Conn), acceptDone: make(chan struct{}),
		connections: make(chan struct{}, max(0, limits.MaxConnections)),
		handshakes:  make(chan struct{}, max(0, limits.MaxConcurrentHandshakes)),
		live:        make(map[*transportConn]struct{}),
	}
	go l.run()

	return l
}

func (l *transportListener) run() {
	defer close(l.acceptDone)

	var retryDelay time.Duration

	for {
		if l.ctx.Err() != nil {
			l.acceptErr = net.ErrClosed
			return
		}

		conn, err := l.Listener.Accept()
		if err != nil {
			// Retry here, not in net/http: once this pump exits, Accept can
			// only replay acceptErr and cannot resume accepting sockets.
			var temporary net.Error
			if errors.As(err, &temporary) && temporary.Temporary() { //nolint:staticcheck // Match net/http's accept-error retry contract, including EMFILE.
				if retryDelay == 0 {
					retryDelay = 5 * time.Millisecond
				} else {
					retryDelay = min(2*retryDelay, time.Second)
				}

				timer := time.NewTimer(retryDelay)
				select {
				case <-timer.C:
				case <-l.ctx.Done():
					timer.Stop()

					l.acceptErr = net.ErrClosed

					return
				}

				continue
			}

			l.acceptErr = err

			return
		}

		retryDelay = 0

		if !take(l.connections) {
			closeTransport(conn)
			continue
		}

		c := &transportConn{Conn: conn, owner: l}
		l.mu.Lock()

		closed := l.closed || l.ctx.Err() != nil
		if !closed {
			l.live[c] = struct{}{}
		}
		l.mu.Unlock()

		if closed || !take(l.handshakes) {
			closeTransport(c)
			continue
		}

		go l.handshake(c)
	}
}

func (l *transportListener) handshake(raw *transportConn) {
	// Only this goroutine releases handshake admission, after HandshakeContext
	// actually returns, never from a TLS configuration/verification callback.
	conn := tls.Server(raw, l.config)
	ctx, cancel := context.WithTimeout(l.ctx, l.timeout)

	err := raw.SetDeadline(time.Now().Add(l.timeout))
	if err == nil {
		err = conn.HandshakeContext(ctx)
	}

	cancel()
	release(l.handshakes)

	if err != nil {
		closeTransport(raw)
		return
	}

	if err := raw.SetDeadline(time.Time{}); err != nil {
		closeTransport(raw)
		return
	}
	// Return the concrete *tls.Conn, not a wrapper: net/http requires that type
	// to populate Request.TLS and enforce client-certificate authentication.
	select {
	case l.ready <- conn:
	case <-l.ctx.Done():
		closeTransport(raw)
	}
}

func (l *transportListener) Accept() (net.Conn, error) {
	select {
	case <-l.acceptDone:
		return nil, l.acceptErr
	case conn := <-l.ready:
		return conn, nil
	}
}

func (l *transportListener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		l.mu.Lock()
		l.closed = true

		connections := make([]*transportConn, 0, len(l.live))
		for conn := range l.live {
			connections = append(connections, conn)
		}
		l.mu.Unlock()
		// Raw close bypasses TLS close-notify, including for handshakes and
		// completed handshakes still waiting to be handed to net/http.
		for _, conn := range connections {
			closeTransport(conn)
		}

		l.closeErr = l.Listener.Close()
	})

	return l.closeErr
}

type transportConn struct {
	net.Conn
	owner *transportListener
	once  sync.Once
	err   error
}

func (c *transportConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.live, c)
		c.owner.mu.Unlock()
		release(c.owner.connections)
	})

	return c.err
}
