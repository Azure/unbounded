// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestAggregatedNodeTokenHeaders(t *testing.T) {
	const token = "mounted-service-account-token"

	for _, transport := range []string{"HTTP", "WebSocket"} {
		t.Run(transport, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get(nodeIdentityTokenHeader) != token {
					t.Errorf("missing or mismatched aggregated authentication headers")
					http.Error(w, "unauthorized", http.StatusUnauthorized)

					return
				}

				if transport == "WebSocket" {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}

					_ = conn.Close(websocket.StatusNormalClosure, "verified")
				}
			}))
			defer server.Close()

			headers := http.Header{}
			setAggregatedNodeTokenHeaders(headers, token)

			if transport == "WebSocket" {
				conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http")+"/apis/status/nodews",
					&websocket.DialOptions{HTTPHeader: headers})
				if err != nil {
					t.Fatal(err)
				}

				_ = conn.Close(websocket.StatusNormalClosure, "done")
			} else {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/apis/status/push", nil)
				if err != nil {
					t.Fatal(err)
				}

				req.Header = headers

				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}

				defer func() { _ = resp.Body.Close() }()

				if resp.StatusCode != http.StatusOK {
					t.Fatalf("HTTP authentication rejected: %d", resp.StatusCode)
				}
			}
		})
	}

	headers := http.Header{}
	setAggregatedNodeTokenHeaders(headers, "")

	if len(headers) != 0 {
		t.Fatalf("empty mounted token produced authentication headers: %v", headers)
	}
}
