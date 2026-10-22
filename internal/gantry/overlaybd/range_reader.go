// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// RangeReader exposes one immutable ACR OverlayBD blob through Gantry's
// node-local exact-range endpoint.
type RangeReader struct {
	client     *http.Client
	requestURL string
	size       int64
}

// NewRangeReader validates the local Gantry endpoint and immutable origin URL.
func NewRangeReader(client *http.Client, endpoint, origin string, size int64) (*RangeReader, error) {
	if client == nil {
		client = http.DefaultClient
	}

	if size <= 0 {
		return nil, fmt.Errorf("overlaybd range reader: invalid size %d", size)
	}

	endpointURL, err := parseGantryEndpoint(endpoint)
	if err != nil {
		return nil, err
	}

	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme != "https" || originURL.Hostname() == "" || originURL.User != nil || originURL.Fragment != "" {
		return nil, fmt.Errorf("overlaybd range reader: invalid ACR origin %q", origin)
	}

	requestURL := strings.TrimRight(endpointURL.String(), "/") + "/" + originURL.String()

	return &RangeReader{client: client, requestURL: requestURL, size: size}, nil
}

func parseGantryEndpoint(endpoint string) (*url.URL, error) {
	endpointURL, err := url.Parse(endpoint)
	if err != nil || endpointURL.Scheme != "http" || endpointURL.Hostname() == "" || endpointURL.RawQuery != "" || endpointURL.Fragment != "" {
		return nil, fmt.Errorf("overlaybd range reader: invalid Gantry endpoint %q", endpoint)
	}

	if host := endpointURL.Hostname(); host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("overlaybd range reader: Gantry endpoint must be loopback")
		}
	}

	return endpointURL, nil
}

// Size returns the immutable remote object size.
func (r *RangeReader) Size() int64 { return r.size }

func (r *RangeReader) ReadAt(p []byte, offset int64) (int, error) {
	return r.ReadAtContext(context.Background(), p, offset)
}

// ReadAtContext reads one exact range and validates Gantry's response framing.
func (r *RangeReader) ReadAtContext(ctx context.Context, p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if offset < 0 {
		return 0, fmt.Errorf("overlaybd range reader: negative offset %d", offset)
	}

	if offset >= r.size {
		return 0, io.EOF
	}

	want := int64(len(p))
	short := false

	if remaining := r.size - offset; want > remaining {
		want = remaining
		short = true
	}

	end := offset + want - 1

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.requestURL, nil)
	if err != nil {
		return 0, fmt.Errorf("overlaybd range reader: build request: %w", err)
	}

	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("overlaybd range reader: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // response body is fully consumed below

	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("overlaybd range reader: status %d", resp.StatusCode)
	}

	wantContentRange := fmt.Sprintf("bytes %d-%d/%d", offset, end, r.size)
	if resp.Header.Get("Content-Range") != wantContentRange {
		return 0, fmt.Errorf("overlaybd range reader: Content-Range %q, want %q", resp.Header.Get("Content-Range"), wantContentRange)
	}

	if value := resp.Header.Get("Content-Length"); value != "" {
		length, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || length != want {
			return 0, fmt.Errorf("overlaybd range reader: Content-Length %q, want %d", value, want)
		}
	}

	n, err := io.ReadFull(resp.Body, p[:want])
	if err != nil {
		return n, fmt.Errorf("overlaybd range reader: body: %w", err)
	}

	var extra [1]byte
	if extraN, extraErr := resp.Body.Read(extra[:]); extraN != 0 || !errors.Is(extraErr, io.EOF) {
		return n, fmt.Errorf("overlaybd range reader: response exceeded requested range")
	}

	if short {
		return n, io.EOF
	}

	return n, nil
}
