// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ClientOptions is copied by NewClient. Its zero value selects safe defaults.
type ClientOptions struct {
	// Concurrency bounds active page requests per operation; zero means 8.
	Concurrency int
	// Timeout bounds an entire HTTP request, including reading its body. Zero
	// leaves the deadline to the operation's context. For sequential streams it
	// bounds the whole stream, including all page requests and downstream writes.
	Timeout time.Duration
	// Header supplies application headers, copied at construction. Protocol-owned
	// headers (Range, validators, encoding, framing and Racer-Origin-Data) cannot
	// be overridden. Use WithOriginData for request-scoped origin input.
	Header http.Header
}

// Client is reusable and safe for concurrent use. Construct it with NewClient.
type Client struct {
	endpoint   string
	http       *http.Client
	header     http.Header
	workers    int
	owned      *http.Transport
	streamPool *streamPool
}

// NewClient connects to an absolute filesystem Unix socket, such as
// /dev/racer/dataset/cache. Targets are exact, already-escaped path/query strings.
func NewClient(endpoint string, options ClientOptions) (*Client, error) {
	if !filepath.IsAbs(endpoint) || len(endpoint) > 107 || strings.ContainsRune(endpoint, 0) {
		return nil, fmt.Errorf("racer: invalid Unix socket path %q", endpoint)
	}

	if options.Timeout < 0 {
		return nil, fmt.Errorf("racer: timeout must be nonnegative")
	}

	workers := options.Concurrency
	if workers == 0 {
		workers = 8
	}

	if workers < 1 {
		return nil, fmt.Errorf("racer: concurrency must be positive")
	}

	h := make(http.Header, len(options.Header))
	for name, values := range options.Header {
		switch strings.ToLower(name) {
		case "range", "if-match", "if-none-match", "if-range", "if-modified-since", "if-unmodified-since", "accept-encoding", "content-length", "transfer-encoding", "host", "connection", "trailer", "te", "upgrade", "racer-origin-data":
			return nil, fmt.Errorf("racer: protocol-owned header %q", name)
		}

		for _, value := range values {
			h.Add(name, value)
		}
	}

	c := &Client{endpoint: "http://localhost", header: h, workers: workers}
	c.streamPool = &streamPool{endpoint: endpoint, limit: workers, timeout: options.Timeout}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	// This pool always dials the configured local socket, never an HTTP proxy.
	c.owned = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", endpoint)
		},
		MaxIdleConns: workers, MaxIdleConnsPerHost: workers,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true,
		MaxResponseHeaderBytes: 8192,
	}
	c.http = &http.Client{Transport: c.owned, Timeout: options.Timeout}

	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return c, nil
}

// CloseIdleConnections releases this client's pool. Active transfers are unaffected.
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

// Download performs HEAD, then concurrent page GETs into dst at object offsets.
// dst must support concurrent non-overlapping WriteAt calls (as *os.File does).
// On failure dst may contain partial data; it is never truncated or closed.
func (c *Client) Download(ctx context.Context, target string, dst io.WriterAt) (Metadata, error) {
	o, err := c.Open(ctx, target)
	if err != nil {
		return Metadata{}, err
	}

	_, err = o.Download(ctx, dst)

	return o.meta, err
}

// Download writes a previously opened snapshot without repeating HEAD. Memory is
// O(Concurrency * 32 KiB), excluding transport buffers. The count includes partial
// writes, not necessarily a contiguous prefix. No automatic version retry occurs.
func (o *Object) Download(ctx context.Context, dst io.WriterAt) (int64, error) {
	if dst == nil {
		return 0, fmt.Errorf("racer: nil destination")
	}

	var written atomic.Int64

	err := o.pages(ctx, 0, o.meta.Size, func(ctx context.Context, start, end int64) error {
		resp, err := o.page(ctx, start, end)
		if err != nil {
			return err
		}
		defer resp.Body.Close() //nolint:errcheck // Response body cleanup; read errors are authoritative.

		buf := copyBuffers.Get().(*[]byte) //nolint:errcheck // The private pool only contains *[]byte.
		defer copyBuffers.Put(buf)

		w := &offsetWriter{dst: dst, offset: start}
		n, err := io.CopyBuffer(w, io.LimitReader(resp.Body, end-start+1), *buf)
		written.Add(n)

		if err == nil && n != end-start+1 {
			err = io.ErrUnexpectedEOF
		}

		return err
	})

	return written.Load(), err
}

type offsetWriter struct {
	dst    io.WriterAt
	offset int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.dst.WriteAt(p, w.offset)
	w.offset += int64(n)

	return n, err
}

// ReadAt reads into p using concurrent GETs split at page boundaries. It follows
// io.ReaderAt's EOF/count rules but accepts an explicit context. On other errors
// the count is the contiguous completed prefix; later portions may be modified.
func (o *Object) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("racer: negative offset")
	}

	if len(p) == 0 {
		return 0, ctx.Err()
	}

	if off >= o.meta.Size {
		return 0, io.EOF
	}

	length := min(int64(len(p)), o.meta.Size-off)
	// Pages are claimed in order and every claimed page records its outcome.
	// The earliest short/failed page bounds the contiguous prefix. If cancellation
	// stops dispatch between pages, the total bounds it instead. This avoids a
	// completion map growing with object size behind a slow first page.
	var total, firstFailure atomic.Int64
	firstFailure.Store(length)

	record := func(start int64, n int, err error) {
		total.Add(int64(n))

		if err != nil {
			at := start - off + int64(n)
			for old := firstFailure.Load(); at < old; old = firstFailure.Load() {
				if firstFailure.CompareAndSwap(old, at) {
					break
				}
			}
		}
	}

	err := o.pages(ctx, off, length, func(ctx context.Context, start, end int64) error {
		resp, err := o.page(ctx, start, end)
		if err != nil {
			record(start, 0, err)
			return err
		}
		defer resp.Body.Close() //nolint:errcheck // Response body cleanup; read errors are authoritative.

		n, err := io.ReadFull(resp.Body, p[start-off:end-off+1])
		record(start, n, err)

		return err
	})
	if err == nil && length < int64(len(p)) {
		err = io.EOF
	}

	return int(min(total.Load(), firstFailure.Load())), err
}

// pages creates only a bounded worker set, never one goroutine per page. Static
// striding isn't used: workers claim the next page as soon as they finish.
func (o *Object) pages(ctx context.Context, off, length int64, fn func(context.Context, int64, int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if length == 0 {
		return nil
	}

	first := off / PageSize
	last := (off + length - 1) / PageSize
	count := last - first + 1

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var (
		cursor atomic.Int64
		wg     sync.WaitGroup
	)

	for i := int64(0); i < min(int64(o.client.workers), count); i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for ctx.Err() == nil {
				index := cursor.Add(1) - 1
				if index >= count {
					return
				}

				start := (first + index) * PageSize
				end := start + min(PageSize, o.meta.Size-start) - 1
				start = max(start, off)

				end = min(end, off+length-1)
				if err := fn(ctx, start, end); err != nil {
					cancel(fmt.Errorf("racer: page at %d: %w", start, err))
					return
				}
			}
		}()
	}

	wg.Wait()

	return context.Cause(ctx)
}

func identityResponse(resp *http.Response) error {
	encoding := resp.Header.Values("Content-Encoding")
	if resp.Uncompressed || len(resp.TransferEncoding) != 0 || len(encoding) > 1 || len(encoding) == 1 && !strings.EqualFold(encoding[0], "identity") {
		return fmt.Errorf("%w: encoded or chunked representation", ErrProtocol)
	}

	return nil
}

func (o *Object) page(ctx context.Context, start, end int64) (*http.Response, error) {
	r, err := o.client.request(ctx, http.MethodGet, o.target)
	if err != nil {
		return nil, err
	}

	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	r.Header.Set("If-Match", o.meta.ETag)

	resp, err := o.client.http.Do(r)
	if err != nil {
		return nil, err
	}

	if err := o.validatePage(resp, start, end); err != nil {
		resp.Body.Close() //nolint:errcheck // Preserve the validation error.
		return nil, err
	}

	return resp, nil
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
