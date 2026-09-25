// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// ClientConfig selects a cache and bounds a Client's resources. Zero numeric
// fields select defaults; negative values and a zero Cache are invalid.
type ClientConfig struct {
	Cache CacheName
	// MaxConnections bounds both connections and live Values (default 16).
	MaxConnections int
	// DialTimeout bounds each Unix dial (default 5 seconds).
	DialTimeout time.Duration
	// ResponseHeaderTimeout starts after request headers are written (default 10 seconds).
	ResponseHeaderTimeout time.Duration
	// IdleConnTimeout bounds pooled idle connections (default 90 seconds).
	IdleConnTimeout time.Duration
}

// Client opens fresh, full-object streams over the cache's canonical Unix socket.
// Get and Close are safe concurrently. Construct with NewClient; do not copy.
type Client struct {
	mu        sync.Mutex
	closed    bool
	active    map[*Value]struct{}
	slots     chan struct{}
	transport *http.Transport
}

// NewClient validates config without dialing. Each client owns its transport and
// connection pool. Get contexts, rather than a total HTTP timeout, bound streams.
func NewClient(config ClientConfig) (*Client, error) {
	return newClient(config, "/run/racer/"+config.Cache.value+"/client/socket")
}

func newClient(config ClientConfig, path string) (*Client, error) {
	if _, err := ParseCacheName(config.Cache.value); err != nil {
		return nil, err
	}

	if config.MaxConnections < 0 || config.DialTimeout < 0 || config.ResponseHeaderTimeout < 0 || config.IdleConnTimeout < 0 {
		return nil, failure(ErrorInvalidArgument, "client config", nil)
	}

	if config.MaxConnections == 0 {
		config.MaxConnections = 16
	}

	if config.DialTimeout == 0 {
		config.DialTimeout = 5 * time.Second
	}

	if config.ResponseHeaderTimeout == 0 {
		config.ResponseHeaderTimeout = 10 * time.Second
	}

	if config.IdleConnTimeout == 0 {
		config.IdleConnTimeout = 90 * time.Second
	}

	dialer := &net.Dialer{Timeout: config.DialTimeout}
	t := &http.Transport{
		Proxy: nil, DisableCompression: true,
		MaxConnsPerHost: config.MaxConnections, MaxIdleConns: config.MaxConnections, MaxIdleConnsPerHost: config.MaxConnections,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout, IdleConnTimeout: config.IdleConnTimeout,
		MaxResponseHeaderBytes: maxHeadBytes,
		Protocols:              new(http.Protocols),
	}
	t.Protocols.SetHTTP1(true)
	t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}

		return newResponseConn(conn), nil
	}

	return &Client{active: make(map[*Value]struct{}), slots: make(chan struct{}, config.MaxConnections), transport: t}, nil
}

// Get admits a fresh object using a bootstrap GET and returns as soon as validated
// headers arrive, without buffering page zero. ctx governs the returned Value's
// entire lifetime, including capacity waits and its lazy pinned continuation.
// The caller owns the Value and should defer Close.
func (c *Client) Get(ctx context.Context, request Request) (*Value, error) {
	if ctx == nil {
		return nil, failure(ErrorInvalidArgument, "get context", nil)
	}

	r := OriginRequest{key: request.Key, context: request.Context, operation: OperationBootstrap, byteRange: bootstrapRange()}
	if _, err := requestHead(r); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	v := &Value{client: c, ctx: ctx, cancel: cancel, request: r, finished: make(chan struct{})}
	c.mu.Lock()
	if c.closed || c.transport == nil {
		c.mu.Unlock()
		cancel()

		return nil, failure(ErrorClosed, "get", nil)
	}

	c.active[v] = struct{}{}
	c.mu.Unlock()

	stop := context.AfterFunc(ctx, func() { v.finish(ioFailure("value", ctx.Err())) })
	defer stopPending(v, stop)

	select {
	case c.slots <- struct{}{}:
		v.mu.Lock()
		if v.terminal != nil {
			v.mu.Unlock()
			<-c.slots

			return nil, v.err()
		}

		v.slot = true
		v.mu.Unlock()
	case <-ctx.Done():
		v.finish(ioFailure("get", ctx.Err()))
		return nil, v.err()
	}

	meta, length, err := v.open(r, nil)
	if err != nil {
		v.finish(err)
		return nil, v.err()
	}

	v.metadata, v.remaining = meta, length
	if err := v.err(); err != nil {
		return nil, err
	}

	return v, nil
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

// Close rejects new work, cancels pending Gets and active Values, closes their
// bodies without draining, and closes idle connections. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true

	values := make([]*Value, 0, len(c.active))
	for v := range c.active {
		values = append(values, v)
	}
	c.mu.Unlock()

	for _, v := range values {
		v.finish(failure(ErrorClosed, "client", nil))
	}

	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}

	return nil
}

func (v *Value) open(r OriginRequest, snapshot *Metadata) (Metadata, int64, error) {
	head, err := requestHead(r)
	if err != nil {
		return Metadata{}, 0, err
	}

	state := &responseState{request: r, snapshot: snapshot}
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		conn, ok := info.Conn.(*responseConn)
		if !ok {
			return
		}

		conn.mu.Lock()
		conn.state = state
		conn.mu.Unlock()
	}}
	ctx := httptrace.WithClientTrace(v.ctx, trace)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://racer"+objectPrefix+r.key.String(), nil)
	if err != nil {
		return Metadata{}, 0, ioFailure("request", err)
	}

	req.Header = headHeaders(head)
	req.Header.Del("Host")
	req.Header["User-Agent"] = nil

	res, err := v.client.transport.RoundTrip(req)
	if err != nil {
		state.mu.Lock()
		wireErr := state.err
		state.mu.Unlock()

		if wireErr != nil {
			return Metadata{}, 0, wireErr
		}

		if v.ctx.Err() != nil {
			return Metadata{}, 0, ioFailure("get", v.ctx.Err())
		}

		var typed *Error
		if errors.As(err, &typed) {
			return Metadata{}, 0, typed
		}

		return Metadata{}, 0, ioFailure("get", err)
	}

	state.mu.Lock()
	result, wireErr := state.result, state.err
	state.mu.Unlock()

	if wireErr != nil {
		closeBody(res.Body)
		return Metadata{}, 0, wireErr
	}

	v.mu.Lock()
	if v.terminal != nil {
		err = v.terminal
		v.mu.Unlock()
		closeBody(res.Body)

		return Metadata{}, 0, err
	}

	v.body = res.Body
	v.mu.Unlock()

	return result.metadata, result.length, nil
}
