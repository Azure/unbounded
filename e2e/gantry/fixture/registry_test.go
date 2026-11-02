// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryAuthAndContentIsolation(t *testing.T) {
	var logs bytes.Buffer

	server := httptest.NewTLSServer(Registry(slog.New(slog.NewJSONHandler(&logs, nil))))
	defer server.Close()

	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/v2/", http.StatusOK},
		{http.MethodHead, "/v2", http.StatusOK},
		{http.MethodGet, "/v2/missing/manifests/sha256:1234", http.StatusNotFound},
		{http.MethodHead, "/v2/missing/manifests/sha256:1234", http.StatusNotFound},
		{http.MethodGet, "/v2/missing/blobs/sha256:1234", http.StatusNotFound},
		{http.MethodPost, "/v2/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/missing/manifests/latest", http.StatusNotFound},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, server.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tc.status || resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
				t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
			}

			if tc.status == http.StatusOK {
				if resp.Header.Get("WWW-Authenticate") != "" {
					t.Fatal("anonymous registry requested credentials")
				}

				if tc.method == http.MethodGet && string(body) != "{}" {
					t.Fatalf("probe response = %q", body)
				}
			}

			if tc.method == http.MethodHead && len(body) != 0 {
				t.Fatalf("HEAD returned body %q", body)
			}
		})
	}

	if !strings.Contains(logs.String(), "fixture registry auth probe") || !strings.Contains(logs.String(), "fixture registry content denied") {
		t.Fatal("fixture did not log auth and fallback requests")
	}
}
