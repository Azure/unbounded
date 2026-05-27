// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// edgeClient is a thin HTTP client around orca's edge listener
// (default port 8443; plain HTTP in dev). Path-style addressing
// only - orca does not implement virtual-hosted bucket parsing.
type edgeClient struct {
	baseURL string
	http    *http.Client
}

// edgeResponse carries everything a subcommand might want to inspect:
// status, headers, the streaming body (for benchmark / verify paths
// that consume gigabytes of bytes), and the parsed size + etag from
// the response headers.
type edgeResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
	Size   int64
	ETag   string
}

func newEdgeClient(baseURL string, timeout time.Duration) *edgeClient {
	return &edgeClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			// Per-request deadline is enforced by the request
			// context; the http.Client timeout is the upper bound
			// for an entire request including body read, which can
			// be longer than a single per-op timeout for large
			// blobs. Capping at 5*timeout gives a safety net
			// without truncating multi-GiB downloads.
			Timeout: 5 * timeout,
		},
	}
}

// Head issues HEAD /<bucket>/<key>.
func (c *edgeClient) Head(ctx context.Context, bucket, key string) (edgeResponse, error) {
	return c.do(ctx, http.MethodHead, "/"+bucket+"/"+key, nil)
}

// Get issues GET /<bucket>/<key>. Caller is responsible for closing
// Body.
func (c *edgeClient) Get(ctx context.Context, bucket, key string) (edgeResponse, error) {
	return c.do(ctx, http.MethodGet, "/"+bucket+"/"+key, nil)
}

// GetRange issues GET /<bucket>/<key> with Range: bytes=<start>-<end>.
// Caller closes Body.
func (c *edgeClient) GetRange(ctx context.Context, bucket, key string, start, end int64) (edgeResponse, error) {
	hdr := http.Header{}
	hdr.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	return c.do(ctx, http.MethodGet, "/"+bucket+"/"+key, hdr)
}

func (c *edgeClient) do(ctx context.Context, method, path string, hdr http.Header) (edgeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return edgeResponse{}, fmt.Errorf("edge: build request: %w", err)
	}

	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return edgeResponse{}, fmt.Errorf("edge: %s %s: %w", method, path, err)
	}

	out := edgeResponse{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   resp.Body,
	}

	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			out.Size = n
		}
	}

	if v := resp.Header.Get("ETag"); v != "" {
		out.ETag = unquoteETag(v)
	}

	// HEAD has no body but Go still allocates a (empty) ReadCloser
	// we should drain so the caller doesn't have to.
	if method == http.MethodHead {
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain best-effort
		_ = resp.Body.Close()                 //nolint:errcheck // close best-effort
		out.Body = nil
	}

	return out, nil
}
