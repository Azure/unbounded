// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTokenReviewDirectStatusAuthorization(t *testing.T) {
	verifier, client, token := testTokenReviewVerifier(t, "unbounded-system", "unbounded-net-node", "node-a", true)
	issuer := testTokenIssuer(t)
	health := &healthState{
		clientset:          client,
		nodeTokenVerifier:  verifier,
		nodeServiceAccount: "unbounded-system:unbounded-net-node",
		statusCache:        NewNodeStatusCache(),
	}
	health.isLeader.Store(true)

	mux := http.NewServeMux()
	registerTokenEndpoints(mux, health, nil, issuer, tokenEndpointConfig{
		nodeServiceAccount: health.nodeServiceAccount,
		verifier:           verifier,
	})
	registerPushHandlers(mux, health, nil, make(chan struct{}, maxConcurrentNodeWS), issuer)

	req := httptest.NewRequest(http.MethodPost, directTokenNodePath, strings.NewReader(`{"serviceAccountToken":"`+token+`"}`))
	req.Header.Set("Authorization", "Bearer "+token)

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("token exchange failed: %d %s", resp.Code, resp.Body.String())
	}

	var exchanged tokenNodeResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &exchanged); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(mux)
	defer server.Close()

	for _, nodeName := range []string{"node-a", "node-b"} {
		t.Run(nodeName, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/status/push", strings.NewReader(
				`{"mode":"full","nodeName":"`+nodeName+`","status":{"nodeInfo":{"name":"`+nodeName+`"}}}`,
			))
			req.Header.Set("Authorization", "Bearer "+exchanged.Token)

			resp := httptest.NewRecorder()
			mux.ServeHTTP(resp, req)

			wantCode := http.StatusOK
			if nodeName != "node-a" {
				wantCode = http.StatusForbidden
			}

			if resp.Code != wantCode {
				t.Fatalf("HTTP status: got %d, want %d: %s", resp.Code, wantCode, resp.Body.String())
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			conn, _, err := websocket.Dial(ctx, server.URL+"/status/nodews", &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": []string{"Bearer " + exchanged.Token}},
			})
			if err != nil {
				t.Fatal(err)
			}

			defer func() {
				if err := conn.CloseNow(); err != nil {
					t.Logf("close websocket: %v", err)
				}
			}()

			if err := conn.Write(ctx, websocket.MessageText, []byte(
				`{"type":"node_status_full","nodeName":"`+nodeName+`","status":{"nodeInfo":{"name":"`+nodeName+`"}}}`,
			)); err != nil {
				t.Fatal(err)
			}

			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}

			var reply struct {
				Type string            `json:"type"`
				Data NodeStatusPushAck `json:"data"`
			}
			if err := json.Unmarshal(body, &reply); err != nil {
				t.Fatal(err)
			}

			if nodeName == "node-a" {
				if reply.Data.Status != "ok" {
					t.Fatalf("own-node update rejected: %s", body)
				}
			} else if reply.Data.Status != "resync_required" || reply.Data.Reason != "node token cannot update another node" {
				t.Fatalf("cross-node update not rejected: %s", body)
			}
		})
	}

	if _, exists := health.statusCache.Get("node-b"); exists {
		t.Fatal("cross-node upload mutated node-b")
	}

	actions := client.Actions()
	if len(actions) != 1 || actions[0].GetResource().Resource != "tokenreviews" {
		t.Fatalf("expected one TokenReview at exchange and no API requests for direct uploads, got %v", actions)
	}
}
