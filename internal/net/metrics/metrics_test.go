// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerReturnsMetrics(t *testing.T) {
	handler := Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	// Default registry should include Go runtime metrics
	if !strings.Contains(body, "go_goroutines") {
		t.Error("expected go_goroutines in /metrics output")
	}
}

func TestRegisterAddsMetricsEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
}

func TestHTTPMiddlewareRecordsMetrics(t *testing.T) {
	mw := NewHTTPMiddleware("test_ns")
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := mw.Wrap("/test", inner)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
}

func TestHTTPMiddlewareSameNamespaceReusesCollectors(t *testing.T) {
	mw1 := NewHTTPMiddleware("reuse_test")

	mw2 := NewHTTPMiddleware("reuse_test")
	if mw1 != mw2 {
		t.Error("expected same middleware instance for same namespace")
	}
}
