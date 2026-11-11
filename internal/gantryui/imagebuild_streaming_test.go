// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestStreamingConversionStatuses(t *testing.T) {
	for _, status := range []string{"Succeeded", "Failed", "Canceled", "", "Unknown"} {
		t.Run(status, func(t *testing.T) {
			err := (&Registry{}).waitStreamingConversion(context.Background(), "repo", streamingOperation{
				ID: "one", Status: status,
			}, func(string) {})
			if (err == nil) != (status == "Succeeded") {
				t.Fatalf("status %q: %v", status, err)
			}
		})
	}

	for _, status := range []string{"Running", "NotStarted"} {
		t.Run(status, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())

			var emitted bool

			err := (&Registry{}).waitStreamingConversion(ctx, "repo", streamingOperation{
				ID: "one", Status: status,
			}, func(string) { emitted = true; cancel() })
			if !emitted || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was not respected: %v", err)
			}

			err = (&Registry{}).waitStreamingConversion(context.Background(), "repo", streamingOperation{
				ID: "../other", Status: status,
			}, func(string) {})
			if err == nil {
				t.Fatal("unsafe operation ID accepted")
			}
		})
	}
}

func TestStreamingConversionPolling(t *testing.T) {
	for _, response := range []string{
		`{"status":"Succeeded"}`,
		`{"id":"two","status":"Succeeded"}`,
		`{"id":"one","status":"Failed","error":{"message":"unsupported image"}}`,
		`not json`,
	} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/acr/v1/repo/_operations/one" {
					t.Errorf("unexpected poll path %s", r.URL.Path)
				}

				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()

			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}

			registry := &Registry{host: u.Host, client: &auth.Client{Client: server.Client()}}

			err = registry.waitStreamingConversion(context.Background(), "repo", streamingOperation{
				ID: "one", Status: "Running",
			}, func(string) {})
			if (err == nil) != (response == `{"status":"Succeeded"}`) {
				t.Fatalf("polling response %s: %v", response, err)
			}
		})
	}
}
