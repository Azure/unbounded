// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package transfer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Client implements ifaces.PeerDialer over HTTP/2 cleartext (h2c).
// Reuse a single Client across all peers - the underlying http2.Transport
// pools per-host connections internally.
type Client struct {
	hc *http.Client
}

// ClientOption tweaks Client construction.
type ClientOption func(*clientOptions)

type clientOptions struct {
	dialTimeout     time.Duration
	requestTimeout  time.Duration
	readIdleTimeout time.Duration
}

// WithDialTimeout sets the TCP dial timeout per peer.
func WithDialTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.dialTimeout = d }
}

// WithRequestTimeout caps total time per request.
func WithRequestTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.requestTimeout = d }
}

// WithReadIdleTimeout configures the h2 ping-based idle stall detector.
func WithReadIdleTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.readIdleTimeout = d }
}

// NewClient builds a Client tuned for peer fetches.
func NewClient(opts ...ClientOption) *Client {
	o := clientOptions{
		dialTimeout:     2 * time.Second,
		requestTimeout:  60 * time.Second,
		readIdleTimeout: 10 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	tr := &http2.Transport{
		// AllowHTTP enables h2c upgrade.
		AllowHTTP: true,
		// DialTLS is reused for non-TLS dials when AllowHTTP is true.
		DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
			d := &net.Dialer{Timeout: o.dialTimeout}
			return d.Dial(network, addr)
		},
		ReadIdleTimeout: o.readIdleTimeout,
	}

	return &Client{
		hc: &http.Client{
			Transport: tr,
			Timeout:   o.requestTimeout,
		},
	}
}

// FetchFromPeer implements ifaces.PeerDialer.
func (c *Client) FetchFromPeer(ctx context.Context, peerAddr string, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	url, err := buildPeerURL(peerAddr, ref)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set(MirroredHeader, "1")
	req.Header.Set("Accept", "*/*")
	// Force h2c - the http.Transport will negotiate over plaintext.
	req.URL.Scheme = "http"

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("peer dial %s: %w", peerAddr, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		size := resp.ContentLength

		return resp.Body, size, nil
	case http.StatusNotFound:
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		return nil, 0, &ifaces.ErrNotFound{Digest: ref.Digest}
	default:
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		return nil, 0, &ifaces.ErrPeerHTTPStatus{PeerAddr: peerAddr, StatusCode: resp.StatusCode}
	}
}

func buildPeerURL(peerAddr string, ref ifaces.OriginRef) (string, error) {
	if peerAddr == "" {
		return "", errors.New("empty peerAddr")
	}

	if ref.Digest.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("unsupported digest %s", ref.Digest.Algorithm())
	}

	kind := "blobs"
	if ref.Kind == ifaces.KindManifest {
		kind = "manifests"
	}

	repo := ref.Repository
	if repo == "" {
		// Peer endpoint doesn't actually use the repo path, but OCI URL
		// shape requires one. Use a placeholder.
		repo = "_"
	}
	// Construct: http://<peerAddr>/v2/<repo>/{manifests|blobs}/<digest>
	return "http://" + peerAddr + "/v2/" + repo + "/" + kind + "/" + ref.Digest.String(), nil
}

// Compile-time check.
var _ ifaces.PeerDialer = (*Client)(nil)
