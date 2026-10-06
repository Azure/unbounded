// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

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

	"github.com/Azure/unbounded/pkg/racersdk/internal/sdkhook"
)

func init() {
	sdkhook.NewClientAt = newClient
	sdkhook.ServeOriginAt = serveOrigin
	sdkhook.OriginDefaults = OriginConfig.defaults
	sdkhook.InvalidOrigin = func() error { return failure(ErrorInvalidArgument, "fake origin", nil) }
}

// ClientConfig selects a volume and bounds a Client's resources. Zero numeric
// fields select defaults; negative values and a zero Volume are invalid.
type ClientConfig struct {
	// Volume selects the canonical Racer endpoint.
	Volume VolumeName
	// MaxConnections bounds bulk connections and live Values (default 64).
	MaxConnections int
	// PageWindow selects default subscription page credits (zero selects two).
	// Each subscription uses one connection regardless of its page credits.
	PageWindow int
	// MetadataConnections reserves a separate Stat connection pool (default 4).
	MetadataConnections int
	// MaxQueuedRequests bounds waiting bulk calls (default 128).
	MaxQueuedRequests int
	// MetadataQueuedRequests reserves a separate bounded Stat queue (default 16).
	MetadataQueuedRequests int
	// SmallObjectConnections reserves connections/live Values for SmallObject reads (default 4).
	SmallObjectConnections int
	// SmallObjectQueuedRequests bounds the independent small-object queue (default 128).
	SmallObjectQueuedRequests int
	// QueueTimeout bounds admission waits (default 5 seconds).
	QueueTimeout time.Duration
	// DialTimeout bounds each Unix dial (default 5 seconds).
	DialTimeout time.Duration
	// ResponseHeaderTimeout bounds request writes and response headers (default 60 seconds).
	// Subscriptions share one deadline for both; Stat starts a fresh header deadline
	// after writing its request.
	ResponseHeaderTimeout time.Duration
	// BodyReadTimeout bounds one body read or bounded socket-transfer chunk
	// (default 60 seconds), not the total object lifetime or caller think time.
	BodyReadTimeout time.Duration
	// IdleConnTimeout bounds pooled idle connections (default 90 seconds).
	IdleConnTimeout time.Duration
	// MaxConnAge bounds connection reuse (default 5 minutes). Each successful dial
	// selects a fixed lifetime uniformly from [75%, 100%] of this value, rounded
	// up to whole nanoseconds. Expiry never interrupts an active response.
	MaxConnAge time.Duration
}

// Client streams immutable objects over the volume's canonical Unix socket.
// Get, Stat and Close are safe concurrently. Construct with NewClient; do not copy.
type Client struct {
	mu                            sync.Mutex
	closed                        bool
	active                        map[*admissionLease]struct{}
	slots                         chan struct{}
	queued                        chan struct{}
	bulk, metadataPool, smallPool connectionPool
	config                        ClientConfig
	path                          string
	ctx                           context.Context
	cancel                        context.CancelFunc
	stats                         clientStats
	copySlots                     chan struct{}
	copyBuffers                   chan *[copyBufferSize]byte
	smallCopySlots                chan struct{}
	smallCopyBuffers              chan *[copyBufferSize]byte
}

// NewClient validates config without dialing. Contexts bound stream lifetimes;
// no total HTTP timeout interrupts a progressing body.
func NewClient(config ClientConfig) (*Client, error) {
	return newClient(config, "/run/racer/"+config.Volume.value+"/client/socket")
}

func newClient(config ClientConfig, path string) (*Client, error) {
	if config.PageWindow < 0 || config.PageWindow > 64 {
		return nil, failure(ErrorInvalidArgument, "page window", nil)
	}

	if _, err := ParseVolumeName(config.Volume.value); err != nil {
		return nil, err
	}

	if config.MaxConnections < 0 || config.MetadataConnections < 0 ||
		config.MaxQueuedRequests < 0 || config.MetadataQueuedRequests < 0 ||
		config.SmallObjectConnections < 0 || config.SmallObjectQueuedRequests < 0 ||
		config.QueueTimeout < 0 || config.DialTimeout < 0 || config.ResponseHeaderTimeout < 0 ||
		config.BodyReadTimeout < 0 || config.IdleConnTimeout < 0 || config.MaxConnAge < 0 {
		return nil, failure(ErrorInvalidArgument, "client config", nil)
	}

	defaultIfZero(&config.MaxConnections, 64)
	defaultIfZero(&config.MetadataConnections, 4)
	defaultIfZero(&config.MaxQueuedRequests, 128)
	defaultIfZero(&config.MetadataQueuedRequests, 16)
	defaultIfZero(&config.SmallObjectConnections, 4)
	defaultIfZero(&config.SmallObjectQueuedRequests, 128)
	defaultIfZero(&config.QueueTimeout, 5*time.Second)
	defaultIfZero(&config.DialTimeout, 5*time.Second)
	defaultIfZero(&config.ResponseHeaderTimeout, 60*time.Second)
	defaultIfZero(&config.BodyReadTimeout, 60*time.Second)
	defaultIfZero(&config.IdleConnTimeout, 90*time.Second)
	defaultIfZero(&config.MaxConnAge, 5*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		active: make(map[*admissionLease]struct{}),
		slots:  make(chan struct{}, config.MaxConnections),
		queued: make(chan struct{}, config.MaxQueuedRequests),
		config: config, path: path, ctx: ctx, cancel: cancel,
	}
	c.bulk.slots = c.slots
	c.bulk.queued = c.queued
	c.copySlots = make(chan struct{}, config.MaxConnections)
	c.copyBuffers = make(chan *[copyBufferSize]byte, config.MaxConnections)
	c.metadataPool.slots = make(chan struct{}, config.MetadataConnections)
	c.metadataPool.queued = make(chan struct{}, config.MetadataQueuedRequests)
	c.smallPool.slots = make(chan struct{}, config.SmallObjectConnections)
	c.smallPool.queued = make(chan struct{}, config.SmallObjectQueuedRequests)
	c.smallCopySlots = make(chan struct{}, config.SmallObjectConnections)
	c.smallCopyBuffers = make(chan *[copyBufferSize]byte, config.SmallObjectConnections)
	c.configurePools(connectionPolicy{})

	return c, nil
}

func defaultIfZero[T ~int | ~int64 | ~uint32](value *T, fallback T) {
	if *value == 0 {
		*value = fallback
	}
}

// configurePools initializes production pools and permits construction-only test policy overrides.
// Call only before any checkout; pool policy is immutable once work begins.
func (c *Client) configurePools(config connectionPolicy) {
	config.Path = c.path

	config.DialTimeout = c.config.DialTimeout
	if config.MaxAge == 0 {
		config.MaxAge = c.config.MaxConnAge
	}

	if config.IdleTimeout == 0 {
		config.IdleTimeout = c.config.IdleConnTimeout
	}

	for _, pool := range c.pools() {
		pool.config = defaultPoolConfig(config)
	}
}

func (c *Client) pools() [3]*connectionPool {
	return [3]*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool}
}

// admit bounds waiters before allocating a Value, derived context, or callback.
func (c *Client) admit(ctx context.Context, pool *connectionPool) (*admissionLease, error) {
	c.mu.Lock()
	closed := c.closed || c.ctx == nil
	c.mu.Unlock()

	if closed {
		return nil, failure(ErrorClosed, "admission", nil)
	}

	if err := ctx.Err(); err != nil {
		return nil, ioFailure("admission", err)
	}

	select {
	case pool.slots <- struct{}{}:
	default:
		select {
		case pool.queued <- struct{}{}:
		default:
			c.stats.queueRejections.Add(1)
			return nil, failure(ErrorUnavailable, "queue full", nil)
		}

		c.stats.queueWaits.Add(1)

		started := time.Now()
		timer := time.NewTimer(c.config.QueueTimeout)

		var err error

		select {
		case pool.slots <- struct{}{}:
		case <-ctx.Done():
			err = ioFailure("admission", ctx.Err())
		case <-c.ctx.Done():
			err = failure(ErrorClosed, "admission", nil)
		case <-timer.C:
			c.stats.queueTimeouts.Add(1)

			err = failure(ErrorDeadline, "queue timeout", context.DeadlineExceeded)
		}

		timer.Stop()
		c.stats.queueWaitNanoseconds.Add(uint64(time.Since(started)))
		<-pool.queued

		if err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	if c.closed || ctx.Err() != nil {
		closed := c.closed

		<-pool.slots
		c.mu.Unlock()

		if closed {
			return nil, failure(ErrorClosed, "admission", nil)
		}

		return nil, ioFailure("admission", ctx.Err())
	}

	ctx, cancel := context.WithCancel(ctx)
	lease := &admissionLease{client: c, pool: pool, ctx: ctx, cancel: cancel, slot: true, finished: make(chan struct{})}
	c.active[lease] = struct{}{}
	c.mu.Unlock()
	stopPending(lease, context.AfterFunc(ctx, func() { lease.finish(ioFailure("value", ctx.Err())) }))

	return lease, nil
}

// Get returns an ordered reader over one subscription. It does not issue HEAD,
// bootstrap, or continuation requests. ctx governs admission and the entire
// Value lifetime. Use OpenPages for unordered page delivery.
func (c *Client) Get(ctx context.Context, request Request, options ...ReadOptions) (*Value, error) {
	return c.get(ctx, request, false, options...)
}

// GetStreaming opens an ordered subscription without starting a buffered page
// receiver. Only WriteToHTTP may consume the returned Value; Read and WriteTo
// return ErrorInvalidArgument without consuming it. Unlike Get, this mode may
// expose an incomplete page prefix on failure, but withholds the selected range's
// final byte until Complete is validated. The caller must Close on every path.
// Options and metadata validation are identical to Get.
func (c *Client) GetStreaming(ctx context.Context, request Request, options ...ReadOptions) (*Value, error) {
	return c.get(ctx, request, true, options...)
}

func (c *Client) get(ctx context.Context, request Request, streaming bool, options ...ReadOptions) (*Value, error) {
	if len(options) > 1 {
		return nil, failure(ErrorInvalidArgument, "get", nil)
	}

	var o ReadOptions
	if len(options) == 1 {
		o = options[0]
	}

	o.Ordered = true

	s, err := c.OpenPages(ctx, request, o)
	if err != nil {
		return nil, err
	}

	s.owner.stream = s

	s.owner.streaming = streaming
	if !streaming {
		s.owner.startOrdered(s)
	}

	return s.owner, nil
}

// Stat obtains fresh full-object metadata using HEAD on separately reserved
// connections, so long-lived bulk streams cannot starve metadata requests.
func (c *Client) Stat(ctx context.Context, request Request) (Metadata, error) {
	if ctx == nil {
		return Metadata{}, failure(ErrorInvalidArgument, "stat", nil)
	}

	r := OriginRequest{key: request.Key, context: request.Context, operation: OperationHead}
	if err := validateRequest(r); err != nil {
		return Metadata{}, err
	}

	v, err := c.admit(ctx, &c.metadataPool)
	if err != nil {
		return Metadata{}, err
	}

	m, err := v.openHead(r)
	if err != nil {
		v.finish(err)
		return Metadata{}, v.err()
	}

	v.finish(io.EOF)

	if err := v.err(); err != io.EOF {
		return Metadata{}, err
	}

	return m, nil
}

func stopPending(lease *admissionLease, stop func() bool) {
	lease.mu.Lock()
	defer lease.mu.Unlock()

	if lease.terminal != nil {
		stop()
	} else {
		lease.stop = stop
	}
}

// Close rejects new work, cancels queued calls and active Values, and closes
// connections without draining. Concurrent calls wait for active cleanup.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	// Seal recycling before active cleanup, preserving closed-before-age policy.
	for _, pool := range c.pools() {
		closeBody(pool)
	}

	for _, buffers := range []chan *[copyBufferSize]byte{c.copyBuffers, c.smallCopyBuffers} {
	drainCopyBuffers:
		for {
			select {
			case <-buffers:
			default:
				break drainCopyBuffers
			}
		}
	}

	values := make([]*admissionLease, 0, len(c.active))
	for v := range c.active {
		values = append(values, v)
	}
	c.mu.Unlock()

	for _, v := range values {
		v.finish(failure(ErrorClosed, "client", nil))
	}

	if c.cancel != nil {
		c.cancel()
	}

	return nil
}

// admissionLease owns admission and cancellation independently of consumption.
// Stat uses it directly; Values attach ordered-reader cleanup under mu.
type admissionLease struct {
	mu       sync.Mutex
	client   *Client
	pool     *connectionPool
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	body     io.Closer
	terminal error
	finished chan struct{}
	slot     bool
	cleanup  func()
}

func (lease *admissionLease) err() error {
	if lease == nil {
		return nil
	}

	lease.mu.Lock()
	defer lease.mu.Unlock()

	return lease.terminal
}

func closeBody(body io.Closer) {
	if body != nil {
		if err := body.Close(); err != nil {
			return
		}
	}
}

func (lease *admissionLease) finish(err error) {
	if lease == nil {
		return
	}

	lease.mu.Lock()
	if lease.terminal != nil {
		done := lease.finished
		lease.mu.Unlock()

		if done != nil {
			<-done
		}

		return
	}

	if lease.finished != nil {
		defer close(lease.finished)
	}

	lease.terminal = err
	body, slot, stop, cleanup := lease.body, lease.slot, lease.stop, lease.cleanup
	lease.body, lease.slot, lease.stop, lease.cleanup = nil, false, nil, nil
	lease.mu.Unlock()

	if stop != nil {
		stop()
	}

	if lease.cancel != nil {
		lease.cancel()
	}

	closeBody(body)

	if cleanup != nil {
		cleanup()
	}

	if lease.client != nil {
		if slot {
			<-lease.pool.slots
		}

		lease.client.mu.Lock()
		delete(lease.client.active, lease)
		lease.client.mu.Unlock()
	}
}

// admitCopy holds scratch capacity until the destination returns, even when
// cancellation has already released the connection lease. It never waits or
// checks ctx: each consumer retains its existing cancellation/error precedence.
func (c *Client) admitCopy(pool *connectionPool) (*[copyBufferSize]byte, func(), error) {
	slots, buffers := c.copySlots, c.copyBuffers
	if pool == &c.smallPool {
		slots, buffers = c.smallCopySlots, c.smallCopyBuffers
	}

	select {
	case slots <- struct{}{}:
	default:
		return nil, nil, failure(ErrorUnavailable, "copy capacity", nil)
	}

	var buf *[copyBufferSize]byte
	select {
	case buf = <-buffers:
	default:
		buf = new([copyBufferSize]byte)
	}

	return buf, func() {
		c.mu.Lock()
		if !c.closed {
			buffers <- buf
		}
		c.mu.Unlock()
		<-slots
	}, nil
}

func (c *Client) connection(ctx context.Context, pool *connectionPool, fresh bool) (*pooledConn, bool, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return nil, false, failure(ErrorClosed, "connection", nil)
	}

	return pool.Get(ctx, fresh)
}

func (lease *admissionLease) openHead(r OriginRequest) (Metadata, error) {
	head, err := clientHead(r)
	if err != nil {
		return Metadata{}, err
	}

	for attempt := range 2 {
		result, started, reused, err := lease.exchangeHead(head, r, attempt != 0)
		if err == nil {
			return result.metadata, nil
		}

		if attempt != 0 || !reused || started || lease.ctx.Err() != nil || !staleConnectionError(err) {
			return Metadata{}, err
		}

		lease.mu.Lock()
		body := lease.body
		lease.body = nil
		lease.mu.Unlock()
		closeBody(body)
		lease.client.stats.retries.Add(1)
	}

	panic("unreachable exchange retry")
}

func (lease *admissionLease) exchangeHead(head []byte, r OriginRequest, fresh bool) (result wireResponse, started, reused bool, err error) {
	conn, reused, err := lease.client.connection(lease.ctx, lease.pool, fresh)
	if err != nil {
		return result, false, reused, err
	}

	body := newConnectionBody(conn)

	lease.mu.Lock()
	if lease.terminal != nil {
		err := lease.terminal
		lease.mu.Unlock()
		closeBody(body)

		return result, false, reused, err
	}

	lease.body = body
	lease.mu.Unlock()
	// Bound blocked request writes as well as response heads, but remove all
	// deadlines before exposing a body. Context cancellation closes the lease.
	if err = conn.SetWriteDeadline(time.Now().Add(lease.client.config.ResponseHeaderTimeout)); err == nil {
		var n int

		n, err = conn.Write(head)
		if err == nil && n != len(head) {
			err = io.ErrShortWrite
		}
	}

	if err == nil {
		err = conn.SetReadDeadline(time.Now().Add(lease.client.config.ResponseHeaderTimeout))
	}

	if err == nil {
		head, err = readHeadBytes(conn.Reader, true)
		started = len(head) != 0
	}

	if err != nil {
		if lease.ctx.Err() != nil {
			err = lease.ctx.Err()
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

// Stats is a fixed-size, credential-free client telemetry snapshot. Counters are
// cumulative since construction; gauges describe the sampling instant. Fields
// are sampled independently and need not form a transaction during concurrent I/O.
type Stats struct {
	// Admission limits are effective defaults, not raw zero-valued configuration.
	BulkLimit, MetadataLimit, SmallObjectLimit                int
	BulkQueueLimit, MetadataQueueLimit, SmallObjectQueueLimit int
	// QueueDepth sums calls waiting in all three independently bounded queues.
	QueueDepth int
	// Per-pool queue depths allow saturation to be diagnosed without request labels.
	BulkQueueDepth        int
	MetadataQueueDepth    int
	SmallObjectQueueDepth int
	// QueueWaits counts calls that entered the bounded admission queue.
	QueueWaits uint64
	// QueueWaitNanoseconds sums completed admission waits, including failures.
	QueueWaitNanoseconds uint64
	// QueueRejections counts calls rejected because the queue was full.
	QueueRejections uint64
	// QueueTimeouts counts waits that exceeded QueueTimeout.
	QueueTimeouts uint64
	// ActiveBulk and ActiveMetadata count occupied admission slots, including dials.
	ActiveBulk     int
	ActiveMetadata int
	// ActiveSmallObjects counts occupied small-object slots, including dials.
	ActiveSmallObjects int
	// Connections counts open SDK connections, both active and idle, across pools.
	Connections int64
	// IdleConnections counts reusable connections currently in all three pools.
	IdleConnections int
	// Dials counts attempted connection dials, including failed attempts.
	Dials uint64
	// ConnectionReuses counts leases taken from any idle pool.
	ConnectionReuses uint64
	// ConnectionRotations counts reusable connections retired at their jittered
	// MaxConnAge, including idle timer expiry. It does not count retries or aborts.
	ConnectionRotations uint64
	// Retries counts single fresh-connection retries after stale pooled failures.
	Retries uint64
	// BytesRead counts body bytes observed as consumed by Values, excluding headers
	// and Stat. It includes buffered bytes read before a later body or writer
	// failure. Streaming splice transfers use the standard library's delivered-byte
	// accounting: on destination failure, source bytes left in a kernel pipe are
	// not observable through a public API and may be missing from this count.
	BytesRead uint64
}

type clientStats struct {
	queueWaits, queueWaitNanoseconds, queueRejections, queueTimeouts atomic.Uint64
	retries, bytesRead                                               atomic.Uint64
}

// Stats returns bounded telemetry without allocating per-request labels or
// depending on a metrics framework. It is safe concurrently with Get, Stat and Close.
func (c *Client) Stats() Stats {
	var connections poolStats

	for _, pool := range c.pools() {
		s := pool.Stats()
		connections.Dials += s.Dials
		connections.ConnectionReuses += s.ConnectionReuses
		connections.ConnectionRotations += s.ConnectionRotations
		connections.Connections += s.Connections
		connections.IdleConnections += s.IdleConnections
	}

	bulk, metadata, small := len(c.bulk.queued), len(c.metadataPool.queued), len(c.smallPool.queued)

	return Stats{
		BulkLimit: cap(c.bulk.slots), MetadataLimit: cap(c.metadataPool.slots), SmallObjectLimit: cap(c.smallPool.slots),
		BulkQueueLimit: cap(c.bulk.queued), MetadataQueueLimit: cap(c.metadataPool.queued), SmallObjectQueueLimit: cap(c.smallPool.queued),
		QueueDepth: bulk + metadata + small, BulkQueueDepth: bulk, MetadataQueueDepth: metadata, SmallObjectQueueDepth: small, QueueWaits: c.stats.queueWaits.Load(),
		QueueWaitNanoseconds: c.stats.queueWaitNanoseconds.Load(),
		QueueRejections:      c.stats.queueRejections.Load(), QueueTimeouts: c.stats.queueTimeouts.Load(),
		ActiveBulk: len(c.slots), ActiveMetadata: len(c.metadataPool.slots),
		ActiveSmallObjects: len(c.smallPool.slots),
		Connections:        connections.Connections, IdleConnections: connections.IdleConnections,
		Dials: connections.Dials, ConnectionReuses: connections.ConnectionReuses,
		ConnectionRotations: connections.ConnectionRotations,
		Retries:             c.stats.retries.Load(), BytesRead: c.stats.bytesRead.Load(),
	}
}

// poolTimer is the cancelable portion of an idle-expiry timer.
type poolTimer interface{ Stop() bool }

// connectionPolicy supplies immutable socket policy and construction-only test hooks.
// Durations must be positive. Nil functions select standard-library behavior.
// Dial overrides the default net.Dialer, including its DialTimeout policy.
// AfterFunc must schedule callbacks asynchronously, never inline.
type connectionPolicy struct {
	Path                             string
	DialTimeout, IdleTimeout, MaxAge time.Duration
	Dial                             func(context.Context, string, string) (net.Conn, error)
	Now                              func() time.Time
	AfterFunc                        func(time.Duration, func()) poolTimer
	Jitter                           func(int64) int64
}

// connectionPool owns idle connections and admission channels. Admission bounds
// live Values, including work before dialing and after socket closure. Expiry
// only prevents reuse; it never interrupts an active lease. Do not copy a pool.
type connectionPool struct {
	mu                       sync.Mutex
	config                   connectionPolicy
	closed                   bool
	idle                     []*pooledConn
	dials, reuses, rotations atomic.Uint64
	connections              atomic.Int64
	slots, queued            chan struct{}
}

func defaultPoolConfig(config connectionPolicy) connectionPolicy {
	if config.Dial == nil {
		config.Dial = (&net.Dialer{Timeout: config.DialTimeout}).DialContext
	}

	if config.Now == nil {
		config.Now = time.Now
	}

	if config.AfterFunc == nil {
		config.AfterFunc = func(d time.Duration, f func()) poolTimer { return time.AfterFunc(d, f) }
	}

	if config.Jitter == nil {
		config.Jitter = rand.Int64N
	}

	return config
}

// pooledConn is an exclusively owned lease with a persistent buffered reader.
// Close releases accounting exactly once, even if the socket close fails.
type pooledConn struct {
	net.Conn
	Reader     *bufio.Reader
	pool       *connectionPool
	timer      poolTimer
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

func (conn *pooledConn) Close() error { return conn.close(false) }

func (conn *pooledConn) retire() {
	if err := conn.close(true); err != nil {
		return
	}
}

func (conn *pooledConn) close(rotation bool) error {
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
func (p *connectionPool) Get(ctx context.Context, fresh bool) (*pooledConn, bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, failure(ErrorClosed, "connection", nil)
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
		return nil, false, ioFailure("dial", err)
	}

	if err := ctx.Err(); err != nil {
		closeBody(conn)
		return nil, false, ioFailure("dial", err)
	}

	p.connections.Add(1)

	return &pooledConn{
		Conn:      conn,
		Reader:    bufio.NewReader(conn),
		pool:      p,
		expiresAt: p.config.Now().Add(jitteredConnAge(p.config.MaxAge, p.config.Jitter)),
	}, false, nil
}

// Recycle transfers a clean lease back to its originating pool. The caller must
// relinquish ownership and must not recycle a closed or already idle lease.
// connectionBody handles validated response ownership and buffered-byte checks.
func (p *connectionPool) Recycle(conn *pooledConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		closeBody(conn)
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
					closeBody(conn)
				}

				return
			}
		}
	})
}

// CloseIdle closes idle connections without preventing later checkouts.
func (p *connectionPool) CloseIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closeIdle()
}

func (p *connectionPool) closeIdle() {
	for _, conn := range p.idle {
		conn.timer.Stop()
		closeBody(conn)
	}

	p.idle = nil
}

// Close rejects new checkouts and closes idle connections. Active and in-flight
// dial leases remain owned by callers, who must close or recycle them.
func (p *connectionPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	p.closeIdle()

	return nil
}

// poolStats samples counters independently of active connection I/O.
type poolStats struct {
	Dials, ConnectionReuses, ConnectionRotations uint64
	Connections                                  int64
	IdleConnections                              int
}

func (p *connectionPool) Stats() poolStats {
	p.mu.Lock()
	defer p.mu.Unlock()

	return poolStats{
		Dials:               p.dials.Load(),
		ConnectionReuses:    p.reuses.Load(),
		ConnectionRotations: p.rotations.Load(),
		Connections:         p.connections.Load(),
		IdleConnections:     len(p.idle),
	}
}

// connectionBody owns a connection lease. Close interrupts reads unless
// SetReusable was called after validating a bodyless response and no bytes remain
// buffered.
type connectionBody struct {
	mu               sync.Mutex
	conn             *pooledConn
	reusable, closed bool
}

func newConnectionBody(conn *pooledConn) *connectionBody { return &connectionBody{conn: conn} }

// SetReusable is safe to race with Close; it never resurrects a closed lease.
func (b *connectionBody) SetReusable(reusable bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.reusable = reusable
}

func (b *connectionBody) Close() error {
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

// staleConnectionError reports EOF/reset/broken-pipe, not timeouts. Retry only
// once on a fresh connection and only before receiving any response bytes.
func staleConnectionError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
