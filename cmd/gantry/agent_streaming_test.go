// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	streamingapi "github.com/Azure/unbounded/internal/gantry/streaming"
)

func TestRouteNodeLocalHandlersPreservesRawStreamingTarget(t *testing.T) {
	t.Parallel()

	rawTarget := "/blobs/https://data.example/account//docker/registry/v2/blobs/sha256/ab/value/data?sig=a%2Bb%2Fc%3D"
	streamingCalled := false
	streamingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamingCalled = true

		if r.RequestURI != rawTarget {
			t.Fatalf("RequestURI = %q, want %q", r.RequestURI, rawTarget)
		}

		w.WriteHeader(http.StatusNoContent)
	})
	mirrorHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("mirror handler called for streaming request")
	})

	req := httptest.NewRequest(http.MethodGet, rawTarget, nil)
	response := httptest.NewRecorder()
	routeNodeLocalHandlers(streamingHandler, mirrorHandler).ServeHTTP(response, req)

	if !streamingCalled || response.Code != http.StatusNoContent {
		t.Fatalf("called/status = %v/%d, want true/204", streamingCalled, response.Code)
	}
}

func TestRouteNodeLocalHandlersFallsBackToMirror(t *testing.T) {
	t.Parallel()

	mirrorCalled := false
	handler := routeNodeLocalHandlers(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("streaming handler called") }),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mirrorCalled = true

			w.WriteHeader(http.StatusOK)
		}),
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v2/", nil))

	if !mirrorCalled || response.Code != http.StatusOK {
		t.Fatalf("called/status = %v/%d, want true/200", mirrorCalled, response.Code)
	}
}

func TestCanonicalOriginResolverPreservesConfiguredEndpoint(t *testing.T) {
	t.Parallel()

	resolver := newCanonicalOriginResolver([]config.UpstreamRegistry{{
		Name:     "app.azurecr.io",
		Endpoint: "http://127.0.0.1:8578/proxy?ns=app.azurecr.io",
	}})
	d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	resolved, ok := resolver(streamingapi.OriginURL{
		Raw:       "https://app.azurecr.io/v2/team/app/blobs/" + d.String(),
		Digest:    d,
		Canonical: true,
	})
	if !ok {
		t.Fatal("canonical origin was not resolved")
	}

	want := "http://127.0.0.1:8578/proxy/v2/team/app/blobs/" + d.String() + "?ns=app.azurecr.io"
	if resolved.Raw != want || resolved.Digest != d || resolved.Canonical {
		t.Fatalf("resolved origin = %+v, want raw %q and signed-origin mode", resolved, want)
	}
}

func TestCanonicalOriginResolverRejectsUnknownRegistry(t *testing.T) {
	t.Parallel()

	resolver := newCanonicalOriginResolver([]config.UpstreamRegistry{{
		Name:     "app.azurecr.io",
		Endpoint: "http://127.0.0.1:8578?ns=app.azurecr.io",
	}})
	d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	if _, ok := resolver(streamingapi.OriginURL{
		Raw:       "https://other.azurecr.io/v2/team/app/blobs/" + d.String(),
		Digest:    d,
		Canonical: true,
	}); ok {
		t.Fatal("unknown registry resolved")
	}
}
