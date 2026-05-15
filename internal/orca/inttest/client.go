// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// Client is a thin HTTP wrapper that targets a single replica's edge
// listener and provides typed helpers (GET, GET-Range, HEAD, LIST) for
// test assertions.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client targeting baseURL (e.g. http://127.0.0.1:34567).
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{},
	}
}

// GetResponse is the result of a GET / HEAD request.
type GetResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// Get fetches the full body of /bucket/key.
func (c *Client) Get(ctx context.Context, t *testing.T, bucket, key string) GetResponse {
	t.Helper()

	return c.do(ctx, t, http.MethodGet, fmt.Sprintf("/%s/%s", bucket, key), nil)
}

// GetRange fetches a byte range from /bucket/key.
func (c *Client) GetRange(ctx context.Context, t *testing.T, bucket, key string, start, end int64) GetResponse {
	t.Helper()

	hdr := http.Header{}
	hdr.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	return c.do(ctx, t, http.MethodGet, fmt.Sprintf("/%s/%s", bucket, key), hdr)
}

// Head issues a HEAD against /bucket/key.
func (c *Client) Head(ctx context.Context, t *testing.T, bucket, key string) GetResponse {
	t.Helper()

	return c.do(ctx, t, http.MethodHead, fmt.Sprintf("/%s/%s", bucket, key), nil)
}

// ListBucketResult mirrors the (subset) S3 ListObjectsV2 XML response
// shape produced by the orca edge handler.
type ListBucketResult struct {
	XMLName  xml.Name `xml:"ListBucketResult"`
	Name     string   `xml:"Name"`
	Prefix   string   `xml:"Prefix"`
	KeyCount int      `xml:"KeyCount"`
	Contents []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
		ETag string `xml:"ETag"`
	} `xml:"Contents"`
}

// List issues a LIST against /bucket/?list-type=2&prefix=<prefix>.
func (c *Client) List(ctx context.Context, t *testing.T, bucket, prefix string) ListBucketResult {
	t.Helper()

	resp := c.do(ctx, t, http.MethodGet,
		fmt.Sprintf("/%s/?list-type=2&prefix=%s", bucket, prefix), nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("LIST status=%d body=%s", resp.Status, string(resp.Body))
	}

	var out ListBucketResult
	if err := xml.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("LIST decode: %v body=%s", err, string(resp.Body))
	}

	return out
}

func (c *Client) do(ctx context.Context, t *testing.T, method, path string, hdr http.Header) GetResponse {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body close best-effort in tests

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return GetResponse{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   body,
	}
}
