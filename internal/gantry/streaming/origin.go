// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
)

const (
	defaultOriginDialTimeout         = 30 * time.Second
	defaultOriginTLSHandshakeTimeout = 10 * time.Second
	defaultOriginIdleConnTimeout     = 90 * time.Second
)

// OriginStatusError reports a non-206 response from the signed origin.
type OriginStatusError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *OriginStatusError) Error() string {
	return fmt.Sprintf("streaming origin returned HTTP %d", e.StatusCode)
}

// OriginClient fetches exact ranges from validated signed origin URLs.
type OriginClient struct {
	hc  *http.Client
	sem chan struct{}
}

type redirectDigestContextKey struct{}

// NewOriginClient constructs a client with independent origin concurrency.
func NewOriginClient(policy URLPolicy, maxConcurrent int, responseHeaderTimeout time.Duration) (*OriginClient, error) {
	if maxConcurrent < 1 {
		return nil, fmt.Errorf("streaming origin concurrency must be positive")
	}

	if responseHeaderTimeout <= 0 {
		return nil, fmt.Errorf("streaming response header timeout must be positive")
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: defaultOriginDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       defaultOriginIdleConnTimeout,
		TLSHandshakeTimeout:   defaultOriginTLSHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	hc := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("streaming origin: too many redirects")
			}

			redirected, err := ParseOriginURL(req.URL.String(), policy)
			if err != nil {
				return err
			}

			expected, ok := req.Context().Value(redirectDigestContextKey{}).(digest.Digest)
			if !ok || redirected.Digest != expected {
				return fmt.Errorf("streaming origin: redirect changed digest")
			}

			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("Proxy-Authorization")

			return nil
		},
	}

	return &OriginClient{hc: hc, sem: make(chan struct{}, maxConcurrent)}, nil
}

// FetchRange fetches exactly requested from origin and returns the complete
// object size reported by Content-Range.
func (c *OriginClient) FetchRange(ctx context.Context, origin OriginURL, requested httprange.Range) (io.ReadCloser, int64, string, error) {
	if _, err := httprange.New(requested.Start, requested.End); err != nil {
		return nil, 0, "", err
	}

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, "", ctx.Err()
	}

	release := func() { <-c.sem }

	ctx = context.WithValue(ctx, redirectDigestContextKey{}, origin.Digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.Raw, nil)
	if err != nil {
		release()

		return nil, 0, "", fmt.Errorf("streaming origin request is invalid")
	}

	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Range", requested.HeaderValue())

	resp, err := c.hc.Do(req)
	if err != nil {
		release()

		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, "", ctxErr
		}

		return nil, 0, "", fmt.Errorf("streaming origin request failed")
	}

	if resp.StatusCode != http.StatusPartialContent {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		_ = resp.Body.Close() //nolint:errcheck // best-effort close

		release()

		return nil, 0, "", &OriginStatusError{StatusCode: resp.StatusCode, RetryAfter: retryAfter}
	}

	if resp.ContentLength < 0 {
		_ = resp.Body.Close() //nolint:errcheck // best-effort close

		release()

		return nil, 0, "", fmt.Errorf("streaming origin response omitted Content-Length")
	}

	total, err := httprange.ValidateResponse(requested, resp.Header.Get("Content-Range"), resp.ContentLength)
	if err != nil {
		_ = resp.Body.Close() //nolint:errcheck // best-effort close

		release()

		return nil, 0, "", fmt.Errorf("streaming origin range response: %w", err)
	}

	return &releaseReadCloser{ReadCloser: resp.Body, release: release}, total, resp.Header.Get("Content-Type"), nil
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

type releaseReadCloser struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releaseReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		r.once.Do(r.release)
	}

	return n, err
}

func (r *releaseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)

	return err
}
