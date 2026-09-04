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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// Client implements ifaces.PeerDialer over HTTP/2 cleartext (h2c).
// Reuse a single Client across all peers - the underlying http2.Transport
// pools per-host connections internally.
type Client struct {
	hc          *http.Client
	onBytesRead func(kind string, bytes int64)
}

// peerMaxReadFrameSize pairs with streamcopy.BufferSize. Large peer response
// writes can then cross HTTP/2 in one frame instead of being split into the
// RFC-default 16 KiB frames. The value is within HTTP/2's 16 KiB-16 MiB range.
const peerMaxReadFrameSize = 1 << 20

// ClientOption tweaks Client construction.
type ClientOption func(*clientOptions)

type clientOptions struct {
	dialTimeout     time.Duration
	requestTimeout  time.Duration
	readIdleTimeout time.Duration
	onBytesRead     func(kind string, bytes int64)
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

// WithClientByteMetrics registers a callback for bytes actually read from peer
// response bodies, including partial failed transfers and retries.
func WithClientByteMetrics(onBytesRead func(kind string, bytes int64)) ClientOption {
	return func(o *clientOptions) { o.onBytesRead = onBytesRead }
}

// NewClient builds a Client tuned for peer fetches.
func NewClient(opts ...ClientOption) *Client {
	o := clientOptions{
		dialTimeout:     2 * time.Second,
		requestTimeout:  time.Hour,
		readIdleTimeout: 10 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	tr := &http2.Transport{
		// AllowHTTP enables h2c upgrade.
		AllowHTTP: true,
		// DialTLSContext is reused for non-TLS dials when AllowHTTP is true.
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			d := &net.Dialer{Timeout: o.dialTimeout}
			return d.DialContext(ctx, network, addr)
		},
		ReadIdleTimeout:  o.readIdleTimeout,
		MaxReadFrameSize: peerMaxReadFrameSize,
	}

	return &Client{
		hc: &http.Client{
			Transport: tr,
			Timeout:   o.requestTimeout,
		},
		onBytesRead: o.onBytesRead,
	}
}

// FetchFromPeer implements ifaces.PeerDialer.
func (c *Client) FetchFromPeer(ctx context.Context, peerAddr string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	if ref.Offset < 0 {
		return nil, 0, "", fmt.Errorf("peer fetch offset %d is negative", ref.Offset)
	}

	url, err := buildPeerURL(peerAddr, ref)
	if err != nil {
		return nil, 0, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, "", err
	}

	req.Header.Set(MirroredHeader, "1")
	req.Header.Set("Accept", "*/*")

	if ref.Offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", ref.Offset))
	}

	if authorization := registryauth.Authorization(ctx); authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	// Force h2c - the http.Transport will negotiate over plaintext.
	req.URL.Scheme = "http"

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("peer dial %s: %w", peerAddr, err)
	}

	switch {
	case resp.StatusCode == http.StatusOK && ref.Offset == 0:
		return c.responseBody(resp, ref.Kind), resp.ContentLength, resp.Header.Get("Content-Type"), nil
	case resp.StatusCode == http.StatusPartialContent && ref.Offset > 0:
		start, end, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != ref.Offset || end < start || size <= end ||
			(resp.ContentLength >= 0 && resp.ContentLength != end-start+1) {
			_ = resp.Body.Close() //nolint:errcheck // best-effort body close

			return nil, 0, "", fmt.Errorf("peer %s returned invalid Content-Range %q for offset %d", peerAddr, resp.Header.Get("Content-Range"), ref.Offset)
		}

		return c.responseBody(resp, ref.Kind), size, resp.Header.Get("Content-Type"), nil
	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		return nil, 0, "", &ifaces.ErrNotFound{Digest: ref.Digest}
	default:
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close

		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{PeerAddr: peerAddr, StatusCode: resp.StatusCode, RetryAfter: retryAfter}
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}

	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}

	return when.Sub(now)
}

func (c *Client) responseBody(resp *http.Response, kind ifaces.OriginRefKind) io.ReadCloser {
	body := resp.Body

	if c.onBytesRead != nil {
		kindLabel := kind.MetricLabel()
		body = &countingReadCloser{
			ReadCloser: body,
			onFinish: func(bytes int64) {
				c.onBytesRead(kindLabel, bytes)
			},
		}
	}

	return body
}

func parseContentRange(value string) (start, end, size int64, ok bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}

	rangeAndSize := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(rangeAndSize) != 2 {
		return 0, 0, 0, false
	}

	bounds := strings.Split(rangeAndSize[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}

	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	end, err = strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	size, err = strconv.ParseInt(rangeAndSize[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	return start, end, size, true
}

type countingReadCloser struct {
	io.ReadCloser
	onFinish func(bytes int64)
	bytes    int64
	once     sync.Once
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)

	if err != nil {
		r.finish()
	}

	return n, err
}

func (r *countingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.finish()

	return err
}

func (r *countingReadCloser) finish() {
	r.once.Do(func() {
		if r.onFinish != nil && r.bytes > 0 {
			r.onFinish(r.bytes)
		}
	})
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
		// shape requires one. Use a valid placeholder so our own transfer
		// server's repository validation accepts the URL.
		repo = "gantry"
	} else if err := oci.ValidateRepositoryName(repo); err != nil {
		return "", err
	}
	// Construct: http://<peerAddr>/v2/<repo>/{manifests|blobs}/<digest>
	return "http://" + peerAddr + "/v2/" + repo + "/" + kind + "/" + ref.Digest.String(), nil
}

// Compile-time check.
var _ ifaces.PeerDialer = (*Client)(nil)
