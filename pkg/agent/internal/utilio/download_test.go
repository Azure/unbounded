// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestURLWithoutQuery(t *testing.T) {
	t.Parallel()

	got := URLWithoutQuery("https://artifacts.example.test/archive?sp=r&sig=secret")
	if got != "https://artifacts.example.test/archive" {
		t.Fatalf("URLWithoutQuery() = %q", got)
	}
}

func TestRedactURLQuery(t *testing.T) {
	t.Parallel()

	got := RedactURLQuery("https://artifacts.example.test/archive?sp=r&sig=secret")
	if got != "https://artifacts.example.test/archive?REDACTED" {
		t.Fatalf("RedactURLQuery() = %q", got)
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
