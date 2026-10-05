// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/connpool"
	"github.com/Azure/unbounded/pkg/racersdk/internal/sdkhook"
)

func init() {
	sdkhook.NewClientAt = newClient
	sdkhook.ServeOriginAt = serveOrigin
	sdkhook.OriginDefaults = OriginConfig.defaults
	sdkhook.InvalidOrigin = func() error { return failure(ErrorInvalidArgument, "fake origin", nil) }
	sdkhook.MaxHeadBytes = maxHeadBytes
}

// ClientConfig selects a cache and bounds a Client's resources. Zero numeric
// fields select defaults; negative values and a zero Cache are invalid.
type ClientConfig struct {
	// Cache selects the canonical Racer endpoint.
	Cache CacheName
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
	// ResponseHeaderTimeout starts after request headers are written (default 60 seconds).
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

// Client streams immutable objects over the cache's canonical Unix socket.
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
	return newClient(config, "/run/racer/"+config.Cache.value+"/client/socket")
}

func newClient(config ClientConfig, path string) (*Client, error) {
	if config.PageWindow < 0 || config.PageWindow > 64 {
		return nil, failure(ErrorInvalidArgument, "page window", nil)
	}

	if _, err := ParseCacheName(config.Cache.value); err != nil {
		return nil, err
	}

	if config.MaxConnections < 0 || config.MetadataConnections < 0 || config.MaxQueuedRequests < 0 || config.MetadataQueuedRequests < 0 || config.SmallObjectConnections < 0 || config.SmallObjectQueuedRequests < 0 || config.QueueTimeout < 0 || config.DialTimeout < 0 || config.ResponseHeaderTimeout < 0 || config.BodyReadTimeout < 0 || config.IdleConnTimeout < 0 || config.MaxConnAge < 0 {
		return nil, failure(ErrorInvalidArgument, "client config", nil)
	}

	if config.MaxConnections == 0 {
		config.MaxConnections = 64
	}

	if config.MetadataConnections == 0 {
		config.MetadataConnections = 4
	}

	if config.MaxQueuedRequests == 0 {
		config.MaxQueuedRequests = 128
	}

	if config.MetadataQueuedRequests == 0 {
		config.MetadataQueuedRequests = 16
	}

	if config.SmallObjectConnections == 0 {
		config.SmallObjectConnections = 4
	}

	if config.SmallObjectQueuedRequests == 0 {
		config.SmallObjectQueuedRequests = 128
	}

	if config.QueueTimeout == 0 {
		config.QueueTimeout = 5 * time.Second
	}

	if config.DialTimeout == 0 {
		config.DialTimeout = 5 * time.Second
	}

	if config.ResponseHeaderTimeout == 0 {
		config.ResponseHeaderTimeout = 60 * time.Second
	}

	if config.BodyReadTimeout == 0 {
		config.BodyReadTimeout = 60 * time.Second
	}

	if config.IdleConnTimeout == 0 {
		config.IdleConnTimeout = 90 * time.Second
	}

	if config.MaxConnAge == 0 {
		config.MaxConnAge = 5 * time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{active: make(map[*admissionLease]struct{}), slots: make(chan struct{}, config.MaxConnections), queued: make(chan struct{}, config.MaxQueuedRequests), config: config, path: path, ctx: ctx, cancel: cancel}
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
	c.configurePools(connpool.Config{})

	return c, nil
}

// configurePools initializes production pools and permits construction-only test policy overrides.
// Call only before any checkout; pool policy is immutable once work begins.
func (c *Client) configurePools(config connpool.Config) {
	config.Path = c.path

	config.DialTimeout = c.config.DialTimeout
	if config.MaxAge == 0 {
		config.MaxAge = c.config.MaxConnAge
	}

	if config.IdleTimeout == 0 {
		config.IdleTimeout = c.config.IdleConnTimeout
	}

	for _, pool := range []*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool} {
		pool.Pool = connpool.New(config)
	}
}

// Admission bounds live Values, not just sockets, so it remains in the SDK.
type connectionPool struct {
	*connpool.Pool
	slots, queued chan struct{}
}

func (c *Client) closeIdleConnections() {
	for _, pool := range []*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool} {
		if pool.Pool != nil {
			pool.CloseIdle()
		}
	}
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
	v := &admissionLease{client: c, pool: pool, ctx: ctx, cancel: cancel, slot: true, finished: make(chan struct{})}
	c.active[v] = struct{}{}
	c.mu.Unlock()
	stopPending(v, context.AfterFunc(ctx, func() { v.finish(ioFailure("value", ctx.Err())) }))

	return v, nil
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

func stopPending(v *admissionLease, stop func() bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.terminal != nil {
		stop()
	} else {
		v.stop = stop
	}
}

// Close rejects new work, cancels queued calls and active Values, and closes
// connections without draining. Concurrent calls wait for active cleanup.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	// Seal recycling before active cleanup, preserving closed-before-age policy.
	for _, pool := range []*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool} {
		if pool.Pool != nil {
			closeBody(pool.Pool)
		}
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

func (v *admissionLease) err() error {
	if v == nil {
		return nil
	}

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

func (v *admissionLease) finish(err error) {
	if v == nil {
		return
	}

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
	body, slot, stop, cleanup := v.body, v.slot, v.stop, v.cleanup
	v.body, v.slot, v.stop, v.cleanup = nil, false, nil, nil
	v.mu.Unlock()

	if stop != nil {
		stop()
	}

	if v.cancel != nil {
		v.cancel()
	}

	closeBody(body)

	if cleanup != nil {
		cleanup()
	}

	if v.client != nil {
		if slot {
			<-v.pool.slots
		}

		v.client.mu.Lock()
		delete(v.client.active, v)
		v.client.mu.Unlock()
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

func (v *admissionLease) openHead(r OriginRequest) (Metadata, error) {
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

func (v *admissionLease) exchangeHead(head []byte, r OriginRequest, fresh bool) (result wireResponse, started, reused bool, err error) {
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
	var connections connpool.Stats

	for _, pool := range []*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool} {
		if pool.Pool == nil {
			continue
		}

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
