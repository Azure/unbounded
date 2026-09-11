// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Azure/unbounded/internal/net/authn"
)

func jsonIdentityCases() []struct {
	name     string
	payload  string
	wantCode int
} {
	return []struct {
		name     string
		payload  string
		wantCode int
	}{
		{"matching full identities", `{"mode":"full","type":"node_status_full","nodeName":"node-a","nodeInfo":{"name":"node-a"},"status":{"nodeInfo":{"name":"node-a"}}}`, http.StatusOK},
		{"matching delta identity", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"nodeInfo":{"name":"node-a","siteName":"updated"}}}`, http.StatusOK},
		{"delta without name", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"nodeInfo":{"siteName":"updated"}}}`, http.StatusOK},
		{"null delta node info", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"nodeInfo":null}}`, http.StatusOK},
		{"unknown delta field ignored", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"NODEINFO":{"name":"node-b"}}}`, http.StatusOK},
		{"other node", `{"mode":"full","type":"node_status_full","nodeName":"node-b","status":{"nodeInfo":{"name":"node-b"}}}`, http.StatusForbidden},
		{"delta renames own entry", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"nodeInfo":{"name":"node-b"}}}`, http.StatusBadRequest},
		{"root identity masks other delta target", `{"mode":"delta","type":"node_status_delta","nodeName":"node-b","nodeInfo":{"name":"node-a"},"delta":{"nodeInfo":{"name":"node-b"}}}`, http.StatusBadRequest},
		{"root identity masks other full target", `{"mode":"full","type":"node_status_full","nodeName":"node-b","nodeInfo":{"name":"node-a"},"status":{"nodeInfo":{"siteName":"changed"}}}`, http.StatusBadRequest},
		{"conflicting full status", `{"mode":"full","type":"node_status_full","nodeName":"node-a","status":{"nodeInfo":{"name":"node-b"}}}`, http.StatusBadRequest},
		{"root conflicts with full status", `{"mode":"full","type":"node_status_full","nodeInfo":{"name":"node-a"},"status":{"nodeInfo":{"name":"node-b"}}}`, http.StatusBadRequest},
		{"mixed-case delta cannot hide name", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"nodeInfo":{"name":"node-b"},"NODEINFO":{"name":"node-a"}}}`, http.StatusBadRequest},
		{"null does not erase full node info", `{"mode":"full","type":"node_status_full","nodeName":"node-a","status":{"nodeInfo":{"name":"node-b"},"nodeInfo":null}}`, http.StatusBadRequest},
		{"null does not erase root node info", `{"mode":"full","type":"node_status_full","nodeName":"node-a","nodeInfo":{"name":"node-b"},"nodeInfo":null,"status":{"nodeInfo":{"name":"node-a"}}}`, http.StatusBadRequest},
		{"legacy full root identity conflict", `{"type":"node_status_full","nodeName":"node-a","nodeInfo":{"name":"node-b"},"nodeInfo":null,"status":{"nodeInfo":{"name":"node-a"}}}`, http.StatusBadRequest},
		{"null does not erase envelope name", `{"mode":"delta","type":"node_status_delta","nodeName":"node-b","nodeName":null,"nodeInfo":{"name":"node-a"},"delta":{"nodeInfo":{"name":"node-b"}}}`, http.StatusBadRequest},
		{"missing identity", `{"mode":"full","type":"node_status_full","status":{"nodeInfo":{}}}`, http.StatusBadRequest},
		{"malformed delta identity", `{"mode":"delta","type":"node_status_delta","nodeName":"node-a","delta":{"nodeInfo":"invalid"}}`, http.StatusBadRequest},
		{"malformed JSON", `{bad`, http.StatusBadRequest},
	}
}

func newJSONIdentityHealth() *healthState {
	h := &healthState{
		statusCache:                 NewNodeStatusCache(),
		nodeServiceAccount:          "unbounded-system:unbounded-net-node",
		registerAggregatedAPIServer: true,
		nodeTokenVerifier: fakeServiceAccountTokenVerifier{
			identity: &authn.KubernetesServiceAccountIdentity{
				Subject:            "system:serviceaccount:unbounded-system:unbounded-net-node",
				Namespace:          "unbounded-system",
				ServiceAccountName: "unbounded-net-node",
				NodeName:           "node-a",
			},
		},
	}
	h.isLeader.Store(true)

	for _, name := range []string{"node-a", "node-b"} {
		h.statusCache.StoreFull(name, NodeStatusResponse{NodeInfo: NodeInfo{Name: name, SiteName: "unchanged"}}, "push")
	}

	return h
}

func assertJSONIdentityCache(t *testing.T, h *healthState, before map[string]*CachedNodeStatus, rejected bool) {
	t.Helper()

	after := h.statusCache.GetAll()
	if len(after) != len(before) {
		t.Fatalf("cache entry count changed: got %d, want %d", len(after), len(before))
	}

	for name, previous := range before {
		current, ok := after[name]
		if !ok || current.Status.NodeInfo.Name != name {
			t.Fatalf("cached identity changed for %s: %+v", name, current)
		}

		if rejected || name == "node-b" {
			if current != previous || current.Revision != previous.Revision || current.Status.NodeInfo.SiteName != "unchanged" {
				t.Fatalf("request mutated protected cache entry %s", name)
			}
		}
	}
}

func TestJSONStatusIdentityHTTP(t *testing.T) {
	proxy, clientTLS := testNodeTokenFrontProxy(t)
	issuer := testTokenIssuer(t)
	token := testNodeToken(t, issuer)

	for _, path := range []string{"/status/push", aggregatedNodeStatusPushPath} {
		for _, tc := range jsonIdentityCases() {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				h := newJSONIdentityHealth()
				before := h.statusCache.GetAll()
				mux := http.NewServeMux()
				registerPushHandlers(mux, h, proxy, make(chan struct{}, maxConcurrentNodeWS), issuer)

				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.payload))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Remote-User", "system:serviceaccount:unbounded-system:unbounded-net-node")
				req.Header.Set(nodeIdentityTokenHeader, "service-account-token")
				req.TLS = clientTLS
				resp := httptest.NewRecorder()
				mux.ServeHTTP(resp, req)

				if resp.Code != tc.wantCode {
					t.Fatalf("expected %d, got %d: %s", tc.wantCode, resp.Code, resp.Body.String())
				}

				assertJSONIdentityCache(t, h, before, tc.wantCode != http.StatusOK)
			})
		}
	}
}

func TestJSONStatusIdentityWebSocket(t *testing.T) {
	proxy, clientTLS := testNodeTokenFrontProxy(t)
	issuer := testTokenIssuer(t)
	token := testNodeToken(t, issuer)

	for _, path := range []string{"/status/nodews", aggregatedNodeStatusWebSocketPath} {
		for _, tc := range jsonIdentityCases() {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				h := newJSONIdentityHealth()
				before := h.statusCache.GetAll()

				var evicted atomic.Bool

				h.registerNodeWS("node-a", func() { evicted.Store(true) })
				h.registerNodeWS("node-b", func() { evicted.Store(true) })

				mux := http.NewServeMux()
				registerPushHandlers(mux, h, proxy, make(chan struct{}, maxConcurrentNodeWS), issuer)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.TLS = clientTLS
					mux.ServeHTTP(w, r)
				}))
				defer server.Close()

				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()

				headers := http.Header{
					"Authorization":         []string{"Bearer " + token},
					"X-Remote-User":         []string{"system:serviceaccount:unbounded-system:unbounded-net-node"},
					nodeIdentityTokenHeader: []string{"service-account-token"},
				}

				conn, _, err := websocket.Dial(ctx, server.URL+path, &websocket.DialOptions{HTTPHeader: headers})
				if err != nil {
					t.Fatal(err)
				}

				defer func() {
					if err := conn.CloseNow(); err != nil {
						t.Logf("close websocket: %v", err)
					}
				}()

				if err := conn.Write(ctx, websocket.MessageText, []byte(tc.payload)); err != nil {
					t.Fatal(err)
				}

				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}

				var reply struct {
					Type string            `json:"type"`
					Data NodeStatusPushAck `json:"data"`
				}
				if err := json.Unmarshal(data, &reply); err != nil {
					t.Fatal(err)
				}

				rejected := tc.wantCode != http.StatusOK
				if rejected {
					if reply.Type != "node_status_resync" || reply.Data.Status != "resync_required" {
						t.Fatalf("expected resync rejection, got %s", data)
					}

					if evicted.Load() {
						t.Fatal("rejected frame evicted an existing node connection")
					}

					if _, _, err := conn.Read(ctx); err == nil {
						t.Fatal("connection remained usable after identity rejection")
					} else if ctx.Err() != nil {
						t.Fatal("connection did not close after identity rejection")
					}
				} else if reply.Type != "node_status_ack" || reply.Data.Status != "ok" {
					t.Fatalf("valid identity rejected: %s", data)
				}

				assertJSONIdentityCache(t, h, before, rejected)
			})
		}
	}
}

func TestJSONStatusHandlersRejectIdentityConflicts(t *testing.T) {
	for _, tc := range jsonIdentityCases() {
		if tc.wantCode != http.StatusBadRequest {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			h := newJSONIdentityHealth()

			before := h.statusCache.GetAll()
			if _, code, err := handleStatusPushRequestWithSource(h, []byte(tc.payload), "push"); code != http.StatusBadRequest || err == nil {
				t.Fatalf("HTTP handler accepted invalid identity: code=%d err=%v", code, err)
			}

			if _, ack := handleNodeStatusWSMessageWithSource(h, []byte(tc.payload), "ws"); ack.Status != "resync_required" {
				t.Fatalf("WebSocket handler accepted invalid identity: %+v", ack)
			}

			assertJSONIdentityCache(t, h, before, true)
		})
	}
}
