// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
