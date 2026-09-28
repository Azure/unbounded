// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHMACTokenManagerIdentityMismatchFallback(t *testing.T) {
	for _, tt := range []struct {
		name         string
		directNode   string
		fallbackNode string
		wantToken    string
	}{
		{"wrong direct node", "node-b", "node-a", "fallback-token"},
		{"missing direct node", "", "node-a", "fallback-token"},
		{"both wrong", "node-b", "node-c", ""},
		{"missing fallback node", "node-b", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tokenPath := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenPath, []byte("service-account-token"), 0o600); err != nil {
				t.Fatal(err)
			}

			var (
				calls   []string
				callsMu sync.Mutex
			)

			expiresAt := time.Now().Add(time.Hour).UTC()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callsMu.Lock()

				calls = append(calls, r.URL.Path)
				callsMu.Unlock()

				response := hmacTokenResponse{Token: "wrong-token", NodeName: tt.directNode, ExpiresAt: expiresAt}

				if r.URL.Path == "/fallback" {
					response.Token = "fallback-token"
					response.NodeName = tt.fallbackNode
				}

				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()

			manager := &hmacTokenManager{
				nodeName:    "node-a",
				saTokenPath: tokenPath,
				tokenURLs:   []string{server.URL + "/direct", server.URL + "/fallback"},
				client:      server.Client(),
			}

			token, err := manager.getToken()
			if tt.wantToken == "" {
				if err == nil || !strings.Contains(err.Error(), "/direct: returned token for node") ||
					!strings.Contains(err.Error(), "/fallback: returned token for node") {
					t.Fatalf("expected both identity errors, got %v", err)
				}

				if !manager.issuedAt.IsZero() || !manager.expiresAt.IsZero() {
					t.Fatal("mismatched response cached token timestamps")
				}
			} else {
				if err != nil {
					t.Fatalf("matching fallback failed: %v", err)
				}

				if manager.issuedAt.IsZero() || !manager.expiresAt.Equal(expiresAt) {
					t.Fatal("matching fallback did not cache token timestamps")
				}

				cached, err := manager.getToken()
				if err != nil || cached != tt.wantToken {
					t.Fatalf("cached token = %q, err = %v", cached, err)
				}
			}

			if token != tt.wantToken || manager.token != tt.wantToken {
				t.Fatalf("returned token %q, cached %q, want %q", token, manager.token, tt.wantToken)
			}

			callsMu.Lock()
			defer callsMu.Unlock()

			if !reflect.DeepEqual(calls, []string{"/direct", "/fallback"}) {
				t.Fatalf("unexpected endpoint order or extra exchange: %v", calls)
			}
		})
	}
}
