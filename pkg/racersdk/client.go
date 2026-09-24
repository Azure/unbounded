// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// ClientOptions is copied by NewClient. Its zero value selects safe defaults.
type ClientOptions struct {
	// Timeout bounds an entire HTTP request, including reading its body. Zero
	// leaves the deadline to the operation's context. For sequential streams it
	// bounds the whole stream, including all page requests and downstream writes.
	Timeout time.Duration
}

// Client is reusable and safe for concurrent use. Construct it with NewClient.
type Client struct {
	endpoint   string
	http       *http.Client
	header     http.Header
	owned      *http.Transport
	streamPool *streamPool
}

// NewClient connects to an absolute filesystem Unix socket, such as
// /run/racer/<name>/client/socket, published in ClusterCache.status.clientSocket.
// Targets are exact, already-escaped path/query strings.
func NewClient(endpoint string, options ClientOptions) (*Client, error) {
	if !filepath.IsAbs(endpoint) || len(endpoint) > 107 || strings.ContainsRune(endpoint, 0) {
		return nil, fmt.Errorf("racer: invalid Unix socket path %q", endpoint)
	}

	if options.Timeout < 0 {
		return nil, fmt.Errorf("racer: timeout must be nonnegative")
	}

	const idle = 8

	c := &Client{endpoint: "http://localhost", header: make(http.Header)}
	c.streamPool = &streamPool{endpoint: endpoint, limit: idle, timeout: options.Timeout}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	// This pool always dials the configured local socket, never an HTTP proxy.
	c.owned = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", endpoint)
		},
		MaxIdleConns: idle, MaxIdleConnsPerHost: idle,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true,
		MaxResponseHeaderBytes: 8192,
	}
	c.http = &http.Client{Transport: c.owned, Timeout: options.Timeout}

	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return c, nil
}

// CloseIdleConnections releases idle connections and Linux splice pipes shared
// by this client and its origin-data views.
// Foreground transfers are uninterrupted;
// their checked-out pipes are closed on return. The client remains usable.
func (c *Client) CloseIdleConnections() {
	if c.streamPool != nil {
		c.streamPool.closeIdle()
	}

	if c.owned != nil {
		c.owned.CloseIdleConnections()
	}
}

func (c *Client) request(ctx context.Context, method, target string) (*http.Request, error) {
	if !validTarget(target) {
		return nil, fmt.Errorf("racer: invalid escaped target %q", target)
	}

	r, err := http.NewRequestWithContext(ctx, method, c.endpoint+target, nil)
	if err != nil {
		return nil, err
	}
	// Reject URL parser rewrites; escaped slashes, dot segments, duplicate query
	// keys, their order and a trailing '?' all belong to Racer's cache identity.
	if r.URL.RequestURI() != target {
		return nil, fmt.Errorf("racer: target does not round-trip verbatim")
	}

	r.Header = c.header.Clone()
	r.Header.Set("Accept-Encoding", "identity")

	return r, nil
}

// Stat performs exactly one HEAD and never fetches payload pages.
func (c *Client) Stat(ctx context.Context, target string) (Metadata, error) {
	r, err := c.request(ctx, http.MethodHead, target)
	if err != nil {
		return Metadata{}, err
	}

	resp, err := c.http.Do(r)
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // Response body cleanup; read errors are authoritative.

	if resp.StatusCode != http.StatusOK {
		return Metadata{}, responseError(r.Method, target, resp)
	}

	if err := identityResponse(resp); err != nil {
		return Metadata{}, err
	}

	tag := resp.Header.Get("ETag")
	if resp.ContentLength < 0 || len(resp.Header.Values("ETag")) > 1 || tag != "" && !validETag(tag) || resp.Header.Get("Content-Range") != "" {
		return Metadata{}, fmt.Errorf("%w: invalid metadata", ErrProtocol)
	}

	if !checksumETag(tag) {
		return Metadata{}, ErrNoValidator
	}

	ttl, err := metadataTTL(resp.Header)
	if err != nil {
		return Metadata{}, err
	}

	contentType, err := boundedField(resp.Header, "Content-Type", 256)
	if err != nil {
		return Metadata{}, err
	}

	return Metadata{Size: resp.ContentLength, ETag: tag, TTL: ttl, ContentType: contentType}, nil
}

// Object is an immutable HEAD snapshot, safe for concurrent reads. It holds no
// connection and needs no Close. Open again to observe a newer representation.
type Object struct {
	client *Client
	target string
	meta   Metadata
}

// Open performs a separate HEAD before any page requests are dispatched.
func (c *Client) Open(ctx context.Context, target string) (*Object, error) {
	m, err := c.Stat(ctx, target)
	if err != nil {
		return nil, err
	}

	return &Object{client: c, target: target, meta: m}, nil
}

// Metadata returns a copy of the HEAD snapshot's metadata.
func (o *Object) Metadata() Metadata {
	m := o.meta
	if m.TTL != nil {
		ttl := *m.TTL
		m.TTL = &ttl
	}

	return m
}

func identityResponse(resp *http.Response) error {
	encoding := resp.Header.Values("Content-Encoding")
	if resp.Uncompressed || len(resp.TransferEncoding) != 0 || len(encoding) > 1 || len(encoding) == 1 && !strings.EqualFold(encoding[0], "identity") {
		return fmt.Errorf("%w: encoded or chunked representation", ErrProtocol)
	}

	return nil
}

func (o *Object) validatePage(resp *http.Response, start, end int64) error {
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return responseError(http.MethodGet, o.target, resp)
	}

	if err := identityResponse(resp); err != nil {
		return err
	}

	if resp.Header.Get("ETag") != o.meta.ETag || len(resp.Header.Values("ETag")) > 1 {
		return ErrVersionChanged
	}

	contentType, err := boundedField(resp.Header, "Content-Type", 256)
	if err != nil {
		return err
	}

	if contentType != o.meta.ContentType {
		return fmt.Errorf("%w: Content-Type changed", ErrProtocol)
	}

	if resp.ContentLength != end-start+1 {
		return fmt.Errorf("%w: incorrect page length", ErrProtocol)
	}

	if resp.StatusCode == http.StatusOK {
		if start != 0 || end != o.meta.Size-1 || resp.Header.Get("Content-Range") != "" {
			return fmt.Errorf("%w: server ignored Range", ErrProtocol)
		}
	} else if len(resp.Header.Values("Content-Range")) != 1 || resp.Header.Get("Content-Range") != contentRange(start, end, o.meta.Size) {
		return fmt.Errorf("%w: incorrect Content-Range", ErrProtocol)
	}

	return nil
}
