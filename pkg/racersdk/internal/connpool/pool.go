// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package connpool owns Unix connection leases, reuse, expiry, and accounting.
// Admission stays with the caller: SDK slots bound live Values, including work
// before dialing and after socket closure, and queues have SDK-specific errors.
// A Pool does not bound active connections or interrupt active leases on expiry.
package connpool

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Timer is the cancelable portion of an idle-expiry timer.
type Timer interface{ Stop() bool }

// Config supplies socket policy and optional deterministic lifecycle seams.
// Durations must be positive. Nil functions select standard-library behavior.
// Dial overrides the default net.Dialer (including its DialTimeout policy).
// AfterFunc must schedule callbacks asynchronously, never inline.
type Config struct {
	Path                             string
	DialTimeout, IdleTimeout, MaxAge time.Duration
	Dial                             func(context.Context, string, string) (net.Conn, error)
	Now                              func() time.Time
	AfterFunc                        func(time.Duration, func()) Timer
	Jitter                           func(int64) int64
}

// ErrClosed indicates checkout after pool shutdown; dial errors are unwrapped.
var ErrClosed = errors.New("connection pool closed")

// Pool manages idle leases. Construct with New and do not copy.
type Pool struct {
	mu                       sync.Mutex
	config                   Config
	closed                   bool
	idle                     []*Conn
	dials, reuses, rotations atomic.Uint64
	connections              atomic.Int64
}

// New constructs a pool without dialing. Config is copied and never mutated.
func New(config Config) *Pool {
	if config.Dial == nil {
		config.Dial = (&net.Dialer{Timeout: config.DialTimeout}).DialContext
	}

	if config.Now == nil {
		config.Now = time.Now
	}

	if config.AfterFunc == nil {
		config.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}

	if config.Jitter == nil {
		config.Jitter = rand.Int64N
	}

	return &Pool{config: config}
}

// Config returns the immutable effective policy, including default functions.
// It can be used to construct another pool with an overridden transport.
func (p *Pool) Config() Config { return p.config }

// Conn is an exclusively owned lease with a persistent buffered reader.
// Close releases accounting exactly once, even if the socket close fails.
type Conn struct {
	net.Conn
	Reader     *bufio.Reader
	pool       *Pool
	timer      Timer
	expiresAt  time.Time
	generation uint64
	once       sync.Once
}

// Integer nanoseconds in [ceil(3*maxAge/4), maxAge], without overflow or a
// zero random bound, even for a one-nanosecond lifetime.
func jitteredConnAge(maxAge time.Duration, int64N func(int64) int64) time.Duration {
	spread := maxAge / 4
	return maxAge - spread + time.Duration(int64N(int64(spread)+1))
}

func (conn *Conn) Close() error { return conn.close(false) }

func (conn *Conn) retire() {
	if err := conn.close(true); err != nil {
		return
	}
}

func (conn *Conn) close(rotation bool) error {
	var err error

	conn.once.Do(func() {
		err = conn.Conn.Close()
		conn.pool.connections.Add(-1)

		if rotation {
			conn.pool.rotations.Add(1)
		}
	})

	return err
}

// Get checks out the most recently idle connection, or dials a new one. Fresh
// bypasses idle reuse. The boolean reports reuse, including a possibly stale
// socket; protocol-aware retry decisions remain the caller's responsibility.
func (p *Pool) Get(ctx context.Context, fresh bool) (*Conn, bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, ErrClosed
	}

	for !fresh && len(p.idle) != 0 {
		n := len(p.idle)
		conn := p.idle[n-1]
		p.idle = p.idle[:n-1]

		conn.timer.Stop()
		conn.timer = nil

		conn.generation++
		if !p.config.Now().Before(conn.expiresAt) {
			conn.retire()
			continue
		}
		p.mu.Unlock()
		p.reuses.Add(1)

		return conn, true, nil
	}
	p.mu.Unlock()
	p.dials.Add(1)

	conn, err := p.config.Dial(ctx, "unix", p.config.Path)
	if err != nil {
		return nil, false, err
	}

	if err := ctx.Err(); err != nil {
		closeQuietly(conn)
		return nil, false, err
	}

	p.connections.Add(1)

	return &Conn{
		Conn:      conn,
		Reader:    bufio.NewReader(conn),
		pool:      p,
		expiresAt: p.config.Now().Add(jitteredConnAge(p.config.MaxAge, p.config.Jitter)),
	}, false, nil
}

// Recycle transfers a clean lease back to its originating pool. The caller must
// relinquish ownership and must not recycle a closed or already idle lease.
// Prefer Body for validated response ownership and buffered-byte checks.
func (p *Pool) Recycle(conn *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		closeQuietly(conn)
		return
	}

	remaining := conn.expiresAt.Sub(p.config.Now())
	if remaining <= 0 {
		conn.retire()
		return
	}

	p.idle = append(p.idle, conn)
	conn.generation++
	generation := conn.generation
	conn.timer = p.config.AfterFunc(min(p.config.IdleTimeout, remaining), func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		if conn.generation != generation {
			return
		}

		for i, candidate := range p.idle {
			if candidate == conn {
				p.idle = append(p.idle[:i], p.idle[i+1:]...)
				if !p.config.Now().Before(conn.expiresAt) {
					conn.retire()
				} else {
					closeQuietly(conn)
				}

				return
			}
		}
	})
}

// CloseIdle closes idle connections without preventing later checkouts.
func (p *Pool) CloseIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closeIdle()
}

func (p *Pool) closeIdle() {
	for _, conn := range p.idle {
		conn.timer.Stop()
		closeQuietly(conn)
	}

	p.idle = nil
}

// Close rejects new checkouts and closes idle connections. Active and in-flight
// dial leases remain owned by callers, who must close or recycle them.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	p.closeIdle()

	return nil
}

// Stats is a bounded snapshot. Counters are cumulative; fields are sampled
// independently, not transactionally with concurrent active connection I/O.
type Stats struct {
	Dials, ConnectionReuses, ConnectionRotations uint64
	Connections                                  int64
	IdleConnections                              int
}

// Stats samples lifecycle counters at the same points as socket ownership.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()

	return Stats{
		Dials:               p.dials.Load(),
		ConnectionReuses:    p.reuses.Load(),
		ConnectionRotations: p.rotations.Load(),
		Connections:         p.connections.Load(),
		IdleConnections:     len(p.idle),
	}
}

// Body owns a connection lease. Close interrupts reads unless SetReusable was
// called after validating a bodyless response and no bytes remain buffered.
type Body struct {
	mu               sync.Mutex
	conn             *Conn
	reusable, closed bool
}

// NewBody takes ownership of conn, initially not reusable.
func NewBody(conn *Conn) *Body { return &Body{conn: conn} }

// SetReusable marks a successfully validated response reusable. It is safe to
// race with Close; marking an already closed body never resurrects its lease.
func (b *Body) SetReusable(reusable bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.reusable = reusable
}

// Close returns a clean reusable connection or closes it, exactly once.
func (b *Body) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true
	if b.reusable && b.conn.Reader.Buffered() == 0 {
		b.conn.pool.Recycle(b.conn)
		return nil
	}

	return b.conn.Close()
}

// StaleError reports EOF/reset/broken-pipe, not timeouts. Retry only once on a
// fresh connection and only when no response byte has been received.
func StaleError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}
