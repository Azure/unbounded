// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/unbounded-kube/internal/net/html"
)

// skipIfFrontendUnbuilt skips the calling test when the embedded frontend
// has no index.html (i.e. `make net-frontend` has not been run on this
// tree). Mirrors the precedent in internal/net/html/pages_test.go.
func skipIfFrontendUnbuilt(t *testing.T) {
	t.Helper()
	if _, err := html.ClusterStatusIndex(); errors.Is(err, fs.ErrNotExist) {
		t.Skip("frontend not built; run `make net-frontend` to enable this test")
	}
}

// TestRegisterDashboardHandlers tests RegisterDashboardHandlers.
func TestRegisterDashboardHandlers(t *testing.T) {
	health := &healthState{}
	health.isLeader.Store(true)
	b := NewWSBroadcaster(health)

	mux := http.NewServeMux()
	// Auth disabled for handler routing tests.
	registerDashboardHandlers(mux, health, b, false, nil, nil, nil)

	t.Run("status page served", func(t *testing.T) {
		skipIfFrontendUnbuilt(t)

		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected /status to return 200, got %d", resp.Code)
		}

		if !strings.Contains(resp.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("expected HTML content-type, got %q", resp.Header().Get("Content-Type"))
		}
	})

	t.Run("status page served with trailing slash", func(t *testing.T) {
		skipIfFrontendUnbuilt(t)

		req := httptest.NewRequest(http.MethodGet, "/status/", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected /status/ to return 200, got %d", resp.Code)
		}

		if !strings.Contains(resp.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("expected HTML content-type, got %q", resp.Header().Get("Content-Type"))
		}
	})

	t.Run("status nested path is not served as status page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status/extra", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusNotFound {
			t.Fatalf("expected /status/extra to return 404, got %d", resp.Code)
		}
	})

	t.Run("websocket endpoint blocks when not leader", func(t *testing.T) {
		health.isLeader.Store(false)
		defer health.isLeader.Store(true)

		req := httptest.NewRequest(http.MethodGet, "/status/ws", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected /status/ws to return 503 when not leader, got %d", resp.Code)
		}
	})

	t.Run("websocket endpoint handles non-upgrade request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status/ws", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code < http.StatusBadRequest {
			t.Fatalf("expected /status/ws non-upgrade request to fail with 4xx/5xx, got %d", resp.Code)
		}
	})

	t.Run("static assets route is registered", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/static/", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code == http.StatusInternalServerError {
			t.Fatalf("expected /static handler to avoid internal server error")
		}
	})
}
