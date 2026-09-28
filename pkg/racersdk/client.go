// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

// ClientConfig selects a cache and bounds a Client's resources. Zero numeric
// fields select defaults; negative values and a zero Cache are invalid.
type ClientConfig struct {
	Cache CacheName
	// MaxConnections bounds bulk connections and live Values (default 64).
	MaxConnections int
	// PageWindow enables ordered concurrent page continuations when greater than one.
	// Zero preserves the single pinned-remainder request; connections remain bounded
	// by MaxConnections, with separate metadata and small-object reservations.
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
	// IdleConnTimeout bounds pooled idle connections (default 90 seconds).
	IdleConnTimeout time.Duration
}

// Client streams immutable objects over the cache's canonical Unix socket.
// Get, Stat and Close are safe concurrently. Construct with NewClient; do not copy.
type Client struct {
	mu                            sync.Mutex
	closed                        bool
	active                        map[*Value]struct{}
	slots                         chan struct{}
	queued                        chan struct{}
	pages                         chan struct{}
	bulk, metadataPool, smallPool connectionPool
	config                        ClientConfig
	path                          string
	ctx                           context.Context
	cancel                        context.CancelFunc
	dial                          func(context.Context, string, string) (net.Conn, error)
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
	if config.PageWindow < 0 {
		return nil, failure(ErrorInvalidArgument, "page window", nil)
	}

	if _, err := ParseCacheName(config.Cache.value); err != nil {
		return nil, err
	}

	if config.MaxConnections < 0 || config.MetadataConnections < 0 || config.MaxQueuedRequests < 0 || config.MetadataQueuedRequests < 0 || config.SmallObjectConnections < 0 || config.SmallObjectQueuedRequests < 0 || config.QueueTimeout < 0 || config.DialTimeout < 0 || config.ResponseHeaderTimeout < 0 || config.IdleConnTimeout < 0 {
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

	if config.IdleConnTimeout == 0 {
		config.IdleConnTimeout = 90 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{active: make(map[*Value]struct{}), slots: make(chan struct{}, config.MaxConnections), queued: make(chan struct{}, config.MaxQueuedRequests), config: config, path: path, ctx: ctx, cancel: cancel}
	c.bulk.slots = c.slots
	c.pages = make(chan struct{}, config.MaxConnections)
	c.bulk.queued = c.queued
	c.copySlots = make(chan struct{}, config.MaxConnections)
	c.copyBuffers = make(chan *[copyBufferSize]byte, config.MaxConnections)
	c.metadataPool.slots = make(chan struct{}, config.MetadataConnections)
	c.metadataPool.queued = make(chan struct{}, config.MetadataQueuedRequests)
	c.smallPool.slots = make(chan struct{}, config.SmallObjectConnections)
	c.smallPool.queued = make(chan struct{}, config.SmallObjectQueuedRequests)
	c.smallCopySlots = make(chan struct{}, config.SmallObjectConnections)
	c.smallCopyBuffers = make(chan *[copyBufferSize]byte, config.SmallObjectConnections)
	c.dial = (&net.Dialer{Timeout: config.DialTimeout}).DialContext

	return c, nil
}

// admit bounds waiters before allocating a Value, derived context, or callback.
func (c *Client) admit(ctx context.Context, pool *connectionPool, r OriginRequest) (*Value, error) {
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
	v := &Value{client: c, pool: pool, ctx: ctx, cancel: cancel, request: r, slot: true, finished: make(chan struct{})}
	c.active[v] = struct{}{}
	c.mu.Unlock()
	stopPending(v, context.AfterFunc(ctx, func() { v.finish(ioFailure("value", ctx.Err())) }))

	return v, nil
}

// Get returns validated headers and an owned stream. Without options it performs
// a fresh bootstrap followed lazily by one pinned remainder. Empty options and
// SmallObject alone preserve bootstrap. Range/pin options select metadata using
// HEAD (or a validated supplied snapshot), then open
// exactly the pinned range without a page-zero body. ctx governs admission and
// the returned Value's entire lifetime.
func (c *Client) Get(ctx context.Context, request Request, options ...ReadOptions) (*Value, error) {
	if ctx == nil || len(options) > 1 {
		return nil, failure(ErrorInvalidArgument, "get", nil)
	}

	r := OriginRequest{key: request.Key, context: request.Context, operation: OperationBootstrap, byteRange: bootstrapRange()}
	if err := validateRequest(r); err != nil {
		return nil, err
	}

	var (
		snapshot   *Metadata
		first, end int64
	)

	var o ReadOptions
	if len(options) == 1 {
		o = options[0]
	}

	pool := &c.bulk
	if o.SmallObject {
		pool = &c.smallPool
	}

	if o.Offset != 0 || o.Length != 0 || o.Pin.value != "" || o.Metadata != nil {
		if uint64(o.Offset) > math.MaxInt64 || uint64(o.Length) > math.MaxInt64 || uint64(o.Length) > math.MaxInt64-uint64(o.Offset) {
			return nil, failure(ErrorInvalidArgument, "read options", nil)
		}

		if o.Pin.value != "" {
			if _, err := ParseETag(o.Pin.value); err != nil {
				return nil, err
			}
		}

		var m Metadata
		if o.Metadata != nil {
			m = *o.Metadata
			if err := m.Validate(); err != nil {
				return nil, err
			}

			if o.Pin.value != "" && o.Pin != m.ETag {
				return nil, failure(ErrorInvalidArgument, "snapshot pin", nil)
			}
		} else {
			var err error

			m, err = c.stat(ctx, request, o.Pin)
			if err != nil {
				return nil, err
			}
		}

		if ByteLength(o.Offset) > m.Size || o.Length > m.Size-ByteLength(o.Offset) {
			return nil, failure(ErrorUnsatisfiableRange, "read options", nil)
		}

		if o.SmallObject && m.Size > PageSize {
			return nil, failure(ErrorInvalidArgument, "small object size", nil)
		}

		first, end = int64(o.Offset), int64(m.Size)
		if o.Length != 0 {
			end = first + int64(o.Length)
		}

		snapshot = &m
		r.operation, r.pin = OperationPinned, m.ETag
		r.byteRange = Range{present: true, first: uint64(first), last: uint64(end - 1)}
	}

	v, err := c.admit(ctx, pool, r)
	if err != nil {
		return nil, err
	}

	if snapshot != nil && first == end {
		v.metadata, v.offset, v.end = *snapshot, first, end
		v.finish(io.EOF)

		if err := v.err(); err != io.EOF {
			return nil, err
		}

		return v, nil
	}

	meta, length, err := v.open(r, snapshot)
	if err != nil {
		v.finish(err)
		return nil, v.err()
	}

	v.metadata, v.remaining, v.offset, v.end = meta, length, first, int64(meta.Size)
	if snapshot != nil {
		v.metadata, v.end = *snapshot, end
	}

	if err := v.err(); err != nil {
		return nil, err
	}

	return v, nil
}

// Stat obtains fresh full-object metadata using HEAD on separately reserved
// connections, so long-lived bulk streams cannot starve metadata requests.
func (c *Client) Stat(ctx context.Context, request Request) (Metadata, error) {
	return c.stat(ctx, request, ETag{})
}

func (c *Client) stat(ctx context.Context, request Request, pin ETag) (Metadata, error) {
	if ctx == nil {
		return Metadata{}, failure(ErrorInvalidArgument, "stat", nil)
	}

	r := OriginRequest{key: request.Key, context: request.Context, operation: OperationHead, pin: pin}
	if err := validateRequest(r); err != nil {
		return Metadata{}, err
	}

	v, err := c.admit(ctx, &c.metadataPool, r)
	if err != nil {
		return Metadata{}, err
	}

	m, _, err := v.open(r, nil)
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

func stopPending(v *Value, stop func() bool) {
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

	values := make([]*Value, 0, len(c.active))
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

	c.closeIdleConnections()

	return nil
}
