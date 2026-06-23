// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package unstore provides an ifaces.OriginPuller that fetches blobs from
// a local unbounded-storage HTTP frontend before falling through to the OCI
// registry. It is a protocol shim: it owns all unbounded-storage wire quirks
// so nothing outside this package needs to know about them.
//
// Critical wire behavior: unbounded-storage does NOT return 404 on a cache
// miss. Its HTTP frontend closes the TCP connection without sending any HTTP
// response. The Go http.Client surfaces this as io.ErrUnexpectedEOF or
// io.EOF. This package maps both to ifaces.FailureNotFound so PriorityChain
// treats it as a clean miss and falls through to the next entry.
package unstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const defaultTimeout = 30 * time.Second

// Client is an ifaces.OriginPuller backed by a local unbounded-storage HTTP
// frontend.
type Client struct {
	endpoint string
	hc       *http.Client
	logger   *slog.Logger
	metrics  clientMetrics
}

// clientMetrics holds optional metric callbacks.
type clientMetrics struct {
	onPull        func(kind string)
	onHit         func(kind string)
	onMiss        func(kind string)
	onUnavailable func()
}

// Option configures a Client.
type Option func(*Client)

// WithLogger attaches a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.logger = l.With(slog.String("subsystem", "unstore"))
		}
	}
}

// WithMetrics registers Prometheus counter callbacks.
//
// pull fires on every Pull attempt (not HEAD - consistent with origin.Client).
// hit fires on 200 OK.
// miss fires on FailureNotFound (connection closed without response).
// unavailable fires on FailureTransient (refused, timeout, non-200 status).
func WithMetrics(
	pull func(kind string),
	hit func(kind string),
	miss func(kind string),
	unavailable func(),
) Option {
	return func(c *Client) {
		c.metrics = clientMetrics{
			onPull:        pull,
			onHit:         hit,
			onMiss:        miss,
			onUnavailable: unavailable,
		}
	}
}

// New constructs a Client. endpoint is the base URL of the unbounded-storage
// HTTP frontend (e.g. "http://127.0.0.1:8080"). timeout is the per-request
// deadline; 0 uses a 30s default.
func New(endpoint string, timeout time.Duration, opts ...Option) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	c := &Client{
		endpoint: endpoint,
		hc:       &http.Client{Timeout: timeout},
		logger:   slog.Default().With(slog.String("subsystem", "unstore")),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Pull fetches the blob or manifest identified by ref from unbounded-storage.
// It constructs the full OCI Distribution Spec URL path from ref and sends a
// GET to the local HTTP frontend.
//
// Wire behavior translation:
//   - 200 OK: returns body and Content-Length.
//   - Connection closed before response (miss): FailureNotFound.
//   - Connection refused or timeout: FailureTransient.
//   - Non-200 status: FailureTransient.
func (c *Client) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	kind := ref.Kind.MetricLabel()

	if c.metrics.onPull != nil {
		c.metrics.onPull(kind)
	}

	u := c.endpoint + ociPath(ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, c.transient(ref, fmt.Errorf("build request: %w", err))
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, c.classifyNetErr(ref, err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, 0, c.transient(ref, fmt.Errorf("HTTP %d", resp.StatusCode))
	}

	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

	if c.metrics.onHit != nil {
		c.metrics.onHit(kind)
	}

	return resp.Body, size, nil
}

// Head fetches metadata for ref without transferring the body.
func (c *Client) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	u := c.endpoint + ociPath(ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return 0, "", c.transient(ref, fmt.Errorf("build request: %w", err))
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, "", c.classifyNetErr(ref, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", c.transient(ref, fmt.Errorf("HTTP %d", resp.StatusCode))
	}

	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

	return size, resp.Header.Get("Content-Type"), nil
}

// ociPath builds the OCI Distribution Spec URL path for ref.
//
//   - KindBlob, KindConfig -> /v2/<repo>/blobs/<digest>
//   - KindManifest         -> /v2/<repo>/manifests/<digest>
//
// ref.Registry is NOT included - the unbounded-storage backend binding is an
// operator/deployment concern.
func ociPath(ref ifaces.OriginRef) string {
	sub := "blobs"
	if ref.Kind == ifaces.KindManifest {
		sub = "manifests"
	}

	return "/v2/" + ref.Repository + "/" + sub + "/" + ref.Digest.String()
}

// classifyNetErr maps a net/http transport error to FailureNotFound (connection
// closed before any response = unbounded-storage miss) or FailureTransient
// (refused, timeout, other transport error).
func (c *Client) classifyNetErr(ref ifaces.OriginRef, err error) *ifaces.OriginError {
	kind := ref.Kind.MetricLabel()

	// io.ErrUnexpectedEOF: server closed connection after request was sent
	// but before any response bytes arrived. This is how unbounded-storage
	// signals a cache miss.
	// io.EOF: server closed cleanly before response.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return c.miss(ref, kind, err)
	}

	// url.Error may wrap EOF or carry a Timeout flag.
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return c.transient(ref, err)
		}

		if errors.Is(ue.Err, io.ErrUnexpectedEOF) || errors.Is(ue.Err, io.EOF) {
			return c.miss(ref, kind, err)
		}
	}

	return c.transient(ref, err)
}

func (c *Client) miss(ref ifaces.OriginRef, kind string, err error) *ifaces.OriginError {
	c.logger.Warn("unstore: cache miss (connection closed before response)",
		slog.String("digest", ref.Digest.String()),
	)

	if c.metrics.onMiss != nil {
		c.metrics.onMiss(kind)
	}

	return &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: err}
}

func (c *Client) transient(ref ifaces.OriginRef, err error) *ifaces.OriginError {
	c.logger.Warn("unstore: transient error",
		slog.String("digest", ref.Digest.String()),
		slog.Any("err", err),
	)

	if c.metrics.onUnavailable != nil {
		c.metrics.onUnavailable()
	}

	return &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: err}
}

// Compile-time check that Client implements ifaces.OriginPuller.
var _ ifaces.OriginPuller = (*Client)(nil)
