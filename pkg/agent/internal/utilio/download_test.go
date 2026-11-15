// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const redirectPromptnessLimit = 5 * time.Second

func TestURLWithoutQuery(t *testing.T) {
	t.Parallel()

	got := URLWithoutQuery("https://artifacts.example.test/archive?sp=r&sig=secret")
	if got != "https://artifacts.example.test/archive" {
		t.Fatalf("URLWithoutQuery() = %q", got)
	}
}

func TestURLWithoutQueryMalformedURL(t *testing.T) {
	t.Parallel()

	first := URLWithoutQuery("https://artifacts.example.test/%one?sig=first-secret")
	second := URLWithoutQuery("https://artifacts.example.test/%two?sig=second-secret")

	if first != "https://artifacts.example.test/%one" {
		t.Fatalf("URLWithoutQuery() = %q", first)
	}

	if second != "https://artifacts.example.test/%two" {
		t.Fatalf("URLWithoutQuery() = %q", second)
	}

	if first == second {
		t.Fatal("malformed URLs collapsed to the same query-free identity")
	}
}

func TestRedactURLQuery(t *testing.T) {
	t.Parallel()

	got := RedactURLQuery("https://artifacts.example.test/archive?sp=r&sig=secret")
	if got != "https://artifacts.example.test/archive?REDACTED" {
		t.Fatalf("RedactURLQuery() = %q", got)
	}
}

func TestValidateHTTPDownloadSourceRedactsInvalidURL(t *testing.T) {
	t.Parallel()

	err := validateHTTPDownloadSource("https://artifacts.example.test/%zz?sig=secret")
	if err == nil {
		t.Fatal("validateHTTPDownloadSource() error = nil, want parse error")
	}

	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("parse error contains signed query secret: %v", err)
	}
}

func TestCheckRedirectNoHTTPSDowngrade(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    string
		to      string
		wantErr bool
	}{
		{name: "HTTPS to HTTPS", from: "https://source.example.test/archive", to: "https://cdn.example.test/archive"},
		{name: "HTTP to HTTPS", from: "http://source.example.test/archive", to: "https://cdn.example.test/archive"},
		{name: "HTTP to HTTP", from: "http://source.example.test/archive", to: "http://cdn.example.test/archive"},
		{name: "HTTPS to HTTP", from: "https://source.example.test/archive", to: "http://cdn.example.test/archive", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			from, err := http.NewRequest(http.MethodGet, tt.from, http.NoBody)
			if err != nil {
				t.Fatalf("create source request: %v", err)
			}

			to, err := http.NewRequest(http.MethodGet, tt.to, http.NoBody)
			if err != nil {
				t.Fatalf("create destination request: %v", err)
			}

			err = CheckRedirectNoHTTPSDowngrade(to, []*http.Request{from})
			if tt.wantErr && err == nil {
				t.Fatal("CheckRedirectNoHTTPSDowngrade() error = nil, want error")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("CheckRedirectNoHTTPSDowngrade() error = %v, want nil", err)
			}
		})
	}
}

func TestCheckRedirectNoHTTPSDowngradeRejectsDowngradeAfterUpgrade(t *testing.T) {
	t.Parallel()

	original, err := http.NewRequest(http.MethodGet, "http://source.example.test/archive", http.NoBody)
	if err != nil {
		t.Fatalf("create original request: %v", err)
	}

	upgraded, err := http.NewRequest(http.MethodGet, "https://cdn.example.test/archive", http.NoBody)
	if err != nil {
		t.Fatalf("create upgraded request: %v", err)
	}

	downgraded, err := http.NewRequest(http.MethodGet, "http://mirror.example.test/archive", http.NoBody)
	if err != nil {
		t.Fatalf("create downgraded request: %v", err)
	}

	if err := CheckRedirectNoHTTPSDowngrade(downgraded, []*http.Request{original, upgraded}); err == nil {
		t.Fatal("CheckRedirectNoHTTPSDowngrade() error = nil, want downgrade error")
	}
}

func TestProbeRemoteHTTPObjectFallbackUsesRange(t *testing.T) {
	t.Parallel()

	var gotRange string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodGet:
			gotRange = r.Header.Get("Range")

			w.WriteHeader(http.StatusPartialContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	if err := ProbeRemoteHTTPObject(context.Background(), server.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotRange != "bytes=0-0" {
		t.Fatalf("GET fallback Range header = %q, want bytes=0-0", gotRange)
	}
}

func TestProbeRemoteHTTPObjectRejectsNoContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := ProbeRemoteHTTPObject(context.Background(), server.URL); err == nil {
		t.Fatal("expected No Content response to fail reachability probe")
	}
}

func TestDownloadFromRemoteCapsRedirectLoop(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer server.Close()

	start := time.Now()

	body, err := downloadFromRemote(context.Background(), server.URL+"/loop")
	elapsed := time.Since(start)

	if body != nil {
		defer func() { _ = body.Close() }()
	}

	if err == nil {
		t.Fatal("downloadFromRemote() error = nil, want redirect limit error")
	}

	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("downloadFromRemote() error = %v, want redirect limit error", err)
	}

	if got := requests.Load(); got != 10 {
		t.Fatalf("redirect loop issued %d requests, want 10", got)
	}

	if elapsed > redirectPromptnessLimit {
		t.Fatalf("redirect loop failed after %v, want prompt failure", elapsed)
	}
}

func TestDownloadFromRemoteRejectsRedirectChainLongerThanTen(t *testing.T) {
	t.Parallel()

	var hops atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hops.Add(1) > 12 {
			_, _ = io.WriteString(w, "payload")
			return
		}

		http.Redirect(w, r, "/chain", http.StatusFound)
	}))
	defer server.Close()

	body, err := downloadFromRemote(context.Background(), server.URL+"/chain")
	if body != nil {
		defer func() { _ = body.Close() }()
	}

	if err == nil {
		t.Fatal("downloadFromRemote() error = nil, want redirect limit error")
	}

	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("downloadFromRemote() error = %v, want redirect limit error", err)
	}

	if got := hops.Load(); got != 10 {
		t.Fatalf("redirect chain issued %d requests, want 10", got)
	}
}

func TestDownloadFromRemoteFollowsShortRedirectChain(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle", http.StatusFound)
		case "/middle":
			http.Redirect(w, r, "/final", http.StatusMovedPermanently)
		case "/final":
			_, _ = io.WriteString(w, "payload")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	body, err := downloadFromRemote(context.Background(), server.URL+"/start")
	if err != nil {
		t.Fatalf("downloadFromRemote() error = %v, want nil", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if string(got) != "payload" {
		t.Fatalf("downloadFromRemote() body = %q, want payload", got)
	}
}

func TestProbeRemoteHTTPObjectCapsRedirectLoop(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer server.Close()

	start := time.Now()
	err := ProbeRemoteHTTPObject(context.Background(), server.URL+"/loop")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ProbeRemoteHTTPObject() error = nil, want redirect limit error")
	}

	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("ProbeRemoteHTTPObject() error = %v, want redirect limit error", err)
	}

	if elapsed > redirectPromptnessLimit {
		t.Fatalf("redirect loop failed after %v, want prompt failure", elapsed)
	}
}

func TestCheckRedirectNoHTTPSDowngradeViaClient(t *testing.T) {
	t.Parallel()

	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "insecure")
	}))
	defer insecure.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, insecure.URL, http.StatusFound)
	}))
	defer secure.Close()

	client := &http.Client{
		Transport:     secure.Client().Transport,
		CheckRedirect: CheckRedirectNoHTTPSDowngrade,
	}

	resp, err := client.Get(secure.URL)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	if err == nil {
		t.Fatal("client.Get() error = nil, want HTTPS downgrade error")
	}

	if !strings.Contains(err.Error(), `refusing redirect from HTTPS to "http"`) {
		t.Fatalf("client.Get() error = %v, want HTTPS downgrade error", err)
	}
}
