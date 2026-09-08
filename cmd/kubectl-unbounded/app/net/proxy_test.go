// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestViewerTokenManagerShouldReuse verifies the proactive-refresh window:
// the cached token must be reused while less than refreshThreshold (90%) of
// its lifetime has elapsed and refreshed once that point is reached.
func TestViewerTokenManagerShouldReuse(t *testing.T) {
	t.Parallel()

	now := time.Now()
	lifetime := 30 * time.Minute

	cases := []struct {
		name      string
		token     string
		issuedAt  time.Time
		expiresAt time.Time
		want      bool
	}{
		{
			name: "no token cached",
			want: true, // shouldReuseLocked is only consulted when token != ""; treat as reuse for the zero value.
		},
		{
			name:      "fresh token (just issued)",
			token:     "t",
			issuedAt:  now,
			expiresAt: now.Add(lifetime),
			want:      true,
		},
		{
			name:      "halfway through lifetime",
			token:     "t",
			issuedAt:  now.Add(-lifetime / 2),
			expiresAt: now.Add(lifetime / 2),
			want:      true,
		},
		{
			name:      "just below 90% threshold",
			token:     "t",
			issuedAt:  now.Add(-time.Duration(float64(lifetime) * 0.89)),
			expiresAt: now.Add(time.Duration(float64(lifetime) * 0.11)),
			want:      true,
		},
		{
			name:      "at 90% threshold",
			token:     "t",
			issuedAt:  now.Add(-time.Duration(float64(lifetime) * 0.90)),
			expiresAt: now.Add(time.Duration(float64(lifetime) * 0.10)),
			want:      false,
		},
		{
			name:      "past expiry",
			token:     "t",
			issuedAt:  now.Add(-2 * lifetime),
			expiresAt: now.Add(-lifetime),
			want:      false,
		},
		{
			name:     "no expiry info reuses indefinitely",
			token:    "t",
			issuedAt: time.Time{},
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := &viewerTokenManager{
				token:     tc.token,
				issuedAt:  tc.issuedAt,
				expiresAt: tc.expiresAt,
			}

			if got := m.shouldReuseLocked(); got != tc.want {
				t.Errorf("shouldReuseLocked() = %v, want %v (issued=%s expires=%s now=%s)",
					got, tc.want, tc.issuedAt, tc.expiresAt, time.Now())
			}
		})
	}
}

// fakeRoundTripper is a deterministic http.RoundTripper for tests.
type fakeRoundTripper struct {
	calls int32 // accessed atomically
	// fn returns the response for the Nth call (0-indexed).
	fn func(callIdx int, req *http.Request) (*http.Response, error)
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	idx := atomic.AddInt32(&f.calls, 1) - 1

	return f.fn(int(idx), req)
}

// TestAuthRetryTransportPassThrough verifies that a successful upstream
// response is returned unchanged and the token is applied exactly once.
func TestAuthRetryTransportPassThrough(t *testing.T) {
	t.Parallel()

	var seenAuth string

	rt := &fakeRoundTripper{
		fn: func(_ int, req *http.Request) (*http.Response, error) {
			seenAuth = req.Header.Get("Authorization")

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     http.Header{},
			}, nil
		},
	}

	a := &authRetryTransport{
		next: rt,
		tokens: &viewerTokenManager{
			token:     "tok-A",
			issuedAt:  time.Now(),
			expiresAt: time.Now().Add(time.Hour),
		},
		errOut: io.Discard,
	}

	req, err := http.NewRequest(http.MethodGet, "http://example/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := a.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if seenAuth != "Bearer tok-A" {
		t.Fatalf("upstream got Authorization=%q, want %q", seenAuth, "Bearer tok-A")
	}

	if got := atomic.LoadInt32(&rt.calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1", got)
	}
}

// TestAuthRetryTransportRetryOn401 verifies that a 401 from the upstream
// triggers exactly one token refresh and exactly one replay, and that the
// replay carries a new Authorization header.
func TestAuthRetryTransportRetryOn401(t *testing.T) {
	t.Parallel()

	// fetchCalls counts how many times the manager refreshed its token.
	var fetchCalls int32

	m := &viewerTokenManager{
		token:     "tok-A",
		issuedAt:  time.Now(),
		expiresAt: time.Now().Add(time.Hour),
		fetch: func() (string, time.Time, error) {
			n := atomic.AddInt32(&fetchCalls, 1)

			return fmt.Sprintf("tok-refreshed-%d", n), time.Now().Add(time.Hour), nil
		},
	}

	var seenAuth []string

	rt := &fakeRoundTripper{
		fn: func(idx int, req *http.Request) (*http.Response, error) {
			seenAuth = append(seenAuth, req.Header.Get("Authorization"))

			if idx == 0 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader("nope")),
					Header:     http.Header{},
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     http.Header{},
			}, nil
		},
	}

	a := &authRetryTransport{next: rt, tokens: m, errOut: io.Discard}

	// Use a body to also exercise the buffer/replay path.
	req, err := http.NewRequest(http.MethodPost, "http://example/test", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := a.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}

	if got := atomic.LoadInt32(&rt.calls); got != 2 {
		t.Fatalf("upstream called %d times, want 2 (one retry on 401)", got)
	}

	if got := atomic.LoadInt32(&fetchCalls); got != 1 {
		t.Fatalf("token fetcher called %d times, want 1", got)
	}

	if len(seenAuth) != 2 {
		t.Fatalf("captured %d auth headers, want 2", len(seenAuth))
	}

	if seenAuth[0] != "Bearer tok-A" {
		t.Errorf("first attempt Authorization = %q, want %q", seenAuth[0], "Bearer tok-A")
	}

	if seenAuth[1] != "Bearer tok-refreshed-1" {
		t.Errorf("retry Authorization = %q, want %q (token must be refreshed)", seenAuth[1], "Bearer tok-refreshed-1")
	}
}

// TestAuthRetryTransportNoRetryOn401WhenAuthSupplied verifies that a
// caller-supplied Authorization header is preserved and not retried,
// since we don't own that token.
func TestAuthRetryTransportNoRetryOn401WhenAuthSupplied(t *testing.T) {
	t.Parallel()

	rt := &fakeRoundTripper{
		fn: func(_ int, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     http.Header{},
			}, nil
		},
	}

	a := &authRetryTransport{
		next: rt,
		tokens: &viewerTokenManager{
			token:     "tok-A",
			issuedAt:  time.Now(),
			expiresAt: time.Now().Add(time.Hour),
		},
		errOut: io.Discard,
	}

	req, err := http.NewRequest(http.MethodGet, "http://example/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	req.Header.Set("Authorization", "Bearer caller-supplied")

	resp, err := a.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no retry)", resp.StatusCode)
	}

	if got := atomic.LoadInt32(&rt.calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (no retry)", got)
	}
}

// TestAuthRetryTransportNoRetryOnUpgrade verifies websocket/upgrade
// requests are not retried (their bodies are not replay-safe and
// ReverseProxy hijacks them).
func TestAuthRetryTransportNoRetryOnUpgrade(t *testing.T) {
	t.Parallel()

	rt := &fakeRoundTripper{
		fn: func(_ int, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     http.Header{},
			}, nil
		},
	}

	a := &authRetryTransport{
		next: rt,
		tokens: &viewerTokenManager{
			token:     "tok-A",
			issuedAt:  time.Now(),
			expiresAt: time.Now().Add(time.Hour),
		},
		errOut: io.Discard,
	}

	req, err := http.NewRequest(http.MethodGet, "http://example/ws", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	resp, err := a.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if got := atomic.LoadInt32(&rt.calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (no retry on upgrade)", got)
	}
}
