// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/sdkhook"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func init() {
	sdkhook.NewClientAt = newClient
	sdkhook.ServeOriginAt = serveOrigin
}

// ClientConfig configures a [Client]. Only Volume is required.
type ClientConfig struct {
	// Volume names the Racer volume, a DNS subdomain such as "blobs". The
	// client connects to /run/racer/<Volume>/client/socket.
	Volume string
	// MaxConnections bounds concurrent [Client.Get] transfers without
	// [ReadOptions.SmallObject]. Zero means 64. SmallObject transfers and
	// [Client.Stat] calls each have a separate four-slot lane. Each lane has a
	// bounded queue; calls fail with [ErrUnavailable] if it is full or the
	// wait times out.
	MaxConnections int
}

// Internal limits. Stat and small-object reads have their own lanes so they
// are never stuck behind large transfers.
const (
	socketPathLimit       = 107
	defaultMaxConnections = 64
	bulkQueue             = 128
	smallConnections      = 4
	smallQueue            = 128
	statConnections       = 4
	statQueue             = 16
	// pageCredits is the number of pages Racer may send ahead of the reader.
	pageCredits = 2
)

type clientLimits struct {
	queueTimeout  time.Duration
	dialTimeout   time.Duration
	headerTimeout time.Duration
	bodyTimeout   time.Duration
	idleTimeout   time.Duration
	maxConnAge    time.Duration
}

var defaultClientLimits = clientLimits{
	queueTimeout:  5 * time.Second,
	dialTimeout:   5 * time.Second,
	headerTimeout: 60 * time.Second,
	bodyTimeout:   60 * time.Second,
	idleTimeout:   90 * time.Second,
	maxConnAge:    5 * time.Minute,
}

// Client reads objects from a Racer volume over its local Unix socket. It is
// safe for concurrent use; create one per volume and share it.
type Client struct {
	path   string
	limits clientLimits

	bulk, small, stat lane

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu     sync.Mutex
	idle   []*clientConn
	active map[context.Context]context.CancelCauseFunc
}

type lane struct {
	slots chan struct{}
	queue chan struct{}
}

func newLane(slots, queue int) lane {
	return lane{slots: make(chan struct{}, slots), queue: make(chan struct{}, queue)}
}

// NewClient returns a client for config.Volume. It does not connect until the
// first request, so Racer need not be running yet.
func NewClient(config ClientConfig) (*Client, error) {
	return newClient(config, "/run/racer/"+config.Volume+"/client/socket")
}

func newClient(config ClientConfig, path string) (*Client, error) {
	if err := validateVolume(config.Volume); err != nil {
		return nil, err
	}

	if config.MaxConnections < 0 {
		return nil, invalid("client config", errors.New("negative MaxConnections"))
	}

	if config.MaxConnections == 0 {
		config.MaxConnections = defaultMaxConnections
	}

	ctx, cancel := context.WithCancelCause(context.Background())

	return &Client{
		path:   path,
		limits: defaultClientLimits,
		bulk:   newLane(config.MaxConnections, bulkQueue),
		small:  newLane(smallConnections, smallQueue),
		stat:   newLane(statConnections, statQueue),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func validateVolume(s string) error {
	if len(s) == 0 || len(s) > 253 || len("/run/racer/"+s+"/origin/socket") > socketPathLimit {
		return invalid("volume", errors.New("invalid volume name"))
	}

	for label := range strings.SplitSeq(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid("volume", errors.New("invalid volume name"))
		}

		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return invalid("volume", errors.New("invalid volume name"))
			}
		}
	}

	return nil
}

// Close closes idle connections and fails in-progress and future calls with
// an error wrapping [net.ErrClosed]. It is safe to call more than once.
func (c *Client) Close() error {
	c.mu.Lock()
	c.cancel(errClosed)

	// Cancel request contexts before returning, so peer shutdown cannot
	// report a transport error while client cancellation is still pending.
	for _, cancel := range c.active {
		cancel(errClosed)
	}

	c.active = nil
	idle := c.idle
	c.idle = nil
	c.mu.Unlock()

	for _, conn := range idle {
		closeQuietly(conn)
	}

	return nil
}

// Stat returns the metadata of the current version of an object without
// reading its contents. Pass the returned ETag to [ReadOptions] to read
// exactly that version later.
func (c *Client) Stat(ctx context.Context, request Request) (Metadata, error) {
	const op = "stat"

	r, err := request.wire(wire.OperationHead)
	if err != nil {
		return Metadata{}, err
	}

	head, err := wire.ClientHead(r)
	if err != nil {
		return Metadata{}, invalid(op, err)
	}

	release, err := c.admit(ctx, &c.stat, op)
	if err != nil {
		return Metadata{}, err
	}
	defer release()

	ctx, done := c.bind(ctx)
	defer done()

	for attempt := 0; ; attempt++ {
		conn, reused, err := c.conn(ctx, op, attempt > 0)
		if err != nil {
			return Metadata{}, err
		}

		response, started, err := c.exchangeHead(ctx, conn, head, r)
		if err == nil {
			if !response.Close && conn.r.Buffered() == 0 {
				c.recycle(conn)
			} else {
				closeQuietly(conn)
			}

			return fromWireMetadata(response.Metadata), nil
		}

		closeQuietly(conn)

		if ctx.Err() != nil {
			return Metadata{}, contextError(op, ctx)
		}

		// A pooled connection may have been closed by Racer while idle.
		// Retry once on a fresh connection if nothing was received.
		if attempt == 0 && reused && !started && staleConnectionError(err) {
			continue
		}

		return Metadata{}, ioFailure(op, err)
	}
}

func (c *Client) exchangeHead(ctx context.Context, conn *clientConn, head []byte, r wire.Request) (wire.Response, bool, error) {
	if err := conn.SetDeadline(time.Now().Add(c.limits.headerTimeout)); err != nil {
		return wire.Response{}, false, err
	}

	// Arm cancellation last so its deadline cannot be overwritten by setup.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) }) //nolint:errcheck // Best effort interrupt.
	defer stop()

	if _, err := conn.Write(head); err != nil {
		return wire.Response{}, false, err
	}

	raw, err := wire.ReadHeadBytes(conn.r, true)
	started := len(raw) != 0

	if err != nil {
		return wire.Response{}, started, err
	}

	response, err := wire.ParseClientHeadResponse(raw, r)
	if err != nil {
		return wire.Response{}, started, err
	}

	if !stop() {
		// The interrupt may have raced with success; the deadline it set
		// makes the connection unusable.
		return wire.Response{}, started, ctx.Err()
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return wire.Response{}, started, err
	}

	return response, started, nil
}

// Get starts reading an object and returns once Racer has accepted the read
// and reported the object's metadata. Read the contents with [Object.Read] or
// [Object.WriteTo] and always call [Object.Close].
//
// Pass at most one [ReadOptions]. ctx applies to the whole transfer, not just the
// call to Get. However, [Object.WriteTo] cannot interrupt a blocked destination
// write unless the writer supports write deadlines.
func (c *Client) Get(ctx context.Context, request Request, options ...ReadOptions) (*Object, error) {
	const op = "get"

	if len(options) > 1 {
		return nil, invalid(op, errors.New("more than one ReadOptions"))
	}

	var o ReadOptions
	if len(options) == 1 {
		o = options[0]
	}

	if o.Offset < 0 || o.Length < 0 || o.Length > math.MaxInt64-o.Offset {
		return nil, invalid(op, errors.New("invalid range"))
	}

	if o.ETag != "" {
		if err := wire.ValidateETag(o.ETag); err != nil {
			return nil, invalid(op, err)
		}
	}

	r, err := request.wire(wire.OperationHead)
	if err != nil {
		return nil, err
	}

	r.Pin = o.ETag
	subscription := wire.SubscriptionOptions{
		Offset:      uint64(o.Offset),
		Length:      uint64(o.Length),
		PageCredits: pageCredits,
		ByteCredits: pageCredits * PageSize,
		Ordered:     true,
		SmallObject: o.SmallObject,
		Pin:         o.ETag,
	}

	head, err := wire.SubscriptionHead(r, subscription)
	if err != nil {
		return nil, invalid(op, err)
	}

	l := &c.bulk
	if o.SmallObject {
		l = &c.small
	}

	release, err := c.admit(ctx, l, op)
	if err != nil {
		return nil, err
	}

	ctx, done := c.bind(ctx)

	object, err := c.open(ctx, head, subscription)
	if err != nil {
		done()
		release()

		return nil, err
	}

	object.cancel = done
	object.admitted = release

	return object, nil
}

func (c *Client) open(ctx context.Context, head []byte, o wire.SubscriptionOptions) (*Object, error) {
	const op = "get"

	// Each transfer gets its own connection, closed when the transfer ends.
	conn, _, err := c.conn(ctx, op, true)
	if err != nil {
		return nil, err
	}

	// Closing the connection interrupts any blocked read or write when ctx
	// ends, including a client Close.
	stop := context.AfterFunc(ctx, func() { closeQuietly(conn) })

	fail := func(err error) (*Object, error) {
		stop()
		closeQuietly(conn)

		if ctx.Err() != nil {
			return nil, contextError(op, ctx)
		}

		return nil, ioFailure(op, err)
	}

	if err := conn.SetDeadline(time.Now().Add(c.limits.headerTimeout)); err != nil {
		return fail(err)
	}

	if _, err := conn.Write(head); err != nil {
		return fail(err)
	}

	raw, err := wire.ReadRawHead(conn.r, true)
	if err != nil {
		return fail(err)
	}

	response, err := wire.ParseSubscriptionResponse(raw, o)
	if err != nil {
		return fail(err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}

	return &Object{
		ctx:      ctx,
		stop:     stop,
		conn:     conn,
		timeout:  c.limits.bodyTimeout,
		metadata: fromWireMetadata(response.Metadata),
		seq:      wire.NewSequence(response.First, response.End, true),
		first:    response.First,
		end:      response.End,
		pages:    response.Pages,
	}, nil
}

// admit waits for a slot in l, bounded by the lane's queue and timeout.
func (c *Client) admit(ctx context.Context, l *lane, op string) (func(), error) {
	if c.ctx.Err() != nil {
		return nil, closedError(op)
	}

	if err := ctx.Err(); err != nil {
		return nil, ioFailure(op, err)
	}

	release := sync.OnceFunc(func() { <-l.slots })

	select {
	case l.slots <- struct{}{}:
		return release, nil
	default:
	}

	select {
	case l.queue <- struct{}{}:
		defer func() { <-l.queue }()
	default:
		return nil, failure(wire.ErrorUnavailable, op, errors.New("too many queued requests"))
	}

	timer := time.NewTimer(c.limits.queueTimeout)
	defer timer.Stop()

	select {
	case l.slots <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, failure(wire.ErrorUnavailable, op, errors.New("timed out waiting for a connection"))
	case <-ctx.Done():
		return nil, ioFailure(op, ctx.Err())
	case <-c.ctx.Done():
		return nil, closedError(op)
	}
}

// bind derives a context that also ends, with cause errClosed, when the client
// is closed.
func (c *Client) bind(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)

	c.mu.Lock()
	if c.ctx.Err() != nil {
		cancel(errClosed)
	} else {
		if c.active == nil {
			c.active = make(map[context.Context]context.CancelCauseFunc)
		}

		c.active[ctx] = cancel
	}
	c.mu.Unlock()

	return ctx, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		delete(c.active, ctx)
		cancel(nil)
	}
}

type clientConn struct {
	net.Conn
	r         *bufio.Reader
	expires   time.Time
	idleSince time.Time
}

// conn returns the most recently idle connection unless fresh is set, or dials
// a new one. The boolean reports reuse.
func (c *Client) conn(ctx context.Context, op string, fresh bool) (*clientConn, bool, error) {
	now := time.Now()

	c.mu.Lock()
	for !fresh && len(c.idle) != 0 {
		conn := c.idle[len(c.idle)-1]
		c.idle = c.idle[:len(c.idle)-1]

		if now.Before(conn.expires) && now.Sub(conn.idleSince) < c.limits.idleTimeout {
			c.mu.Unlock()
			return conn, true, nil
		}

		closeQuietly(conn)
	}
	c.mu.Unlock()

	dialer := net.Dialer{Timeout: c.limits.dialTimeout}

	nc, err := dialer.DialContext(ctx, "unix", c.path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, contextError(op, ctx)
		}

		return nil, false, failure(wire.ErrorUnavailable, op, err)
	}

	// Rotate connections over time so Racer restarts and rebalancing take
	// effect. Jitter spreads rotations over [75%, 100%] of the maximum age.
	age := c.limits.maxConnAge
	age -= time.Duration(rand.Int64N(int64(age/4) + 1))

	return &clientConn{Conn: nc, r: bufio.NewReader(nc), expires: now.Add(age)}, false, nil
}

func (c *Client) recycle(conn *clientConn) {
	conn.idleSince = time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ctx.Err() != nil || len(c.idle) >= statConnections {
		closeQuietly(conn)
		return
	}

	c.idle = append(c.idle, conn)
}

func closeQuietly(c io.Closer) {
	if c == nil {
		return
	}

	// The resource is being discarded; its close error carries no signal.
	_ = c.Close() //nolint:errcheck // See above.
}

// staleConnectionError reports EOF, reset, or broken pipe, not timeouts.
func staleConnectionError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
