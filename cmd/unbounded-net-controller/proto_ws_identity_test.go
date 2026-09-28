// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func TestProtoWSIdentityBeforeMutation(t *testing.T) {
	full := func(envelope, nested string) *statusproto.NodeStatusMessage {
		return &statusproto.NodeStatusMessage{
			Type: "node_status_full", NodeName: envelope,
			Status: &statusproto.NodeStatusFull{
				NodeInfo: &statusproto.NodeInfo{Name: nested, SiteName: "updated"},
			},
		}
	}
	cases := []struct {
		name       string
		message    *statusproto.NodeStatusMessage
		corrupt    bool
		reject     bool
		wantClosed bool
	}{
		{name: "matching full", message: full("node-a", "node-a")},
		{name: "nested identity", message: full("", "node-a")},
		{name: "matching delta", message: &statusproto.NodeStatusMessage{
			Type: "node_status_delta", NodeName: "node-a", BaseRevision: 1,
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"},
				NodeInfo:      &statusproto.NodeInfo{Name: "node-a", SiteName: "updated"},
			},
		}},
		{name: "wrong authenticated node", message: full("node-b", "node-b"), reject: true, wantClosed: true},
		{name: "conflicting full", message: full("node-a", "node-b"), reject: true},
		{name: "missing identity", message: full("", ""), reject: true},
		{name: "malformed after valid identity", message: full("node-a", "node-a"), corrupt: true, reject: true},
		{name: "conflicting delta", reject: true, message: &statusproto.NodeStatusMessage{
			Type: "node_status_delta", NodeName: "node-a", BaseRevision: 1,
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"}, NodeInfo: &statusproto.NodeInfo{Name: "node-b"},
			},
		}},
		{name: "conflicting unused full field", reject: true, message: &statusproto.NodeStatusMessage{
			Type: "node_status_delta", NodeName: "node-a", BaseRevision: 1,
			Status: &statusproto.NodeStatusFull{NodeInfo: &statusproto.NodeInfo{Name: "node-b"}},
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"}, NodeInfo: &statusproto.NodeInfo{Name: "node-a"},
			},
		}},
	}
	proxy, clientTLS := testNodeTokenFrontProxy(t)
	issuer := testTokenIssuer(t)
	token := testNodeToken(t, issuer)

	for _, path := range []string{"/status/nodews", aggregatedNodeStatusWebSocketPath} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				health := newJSONIdentityHealth()
				before := health.statusCache.GetAll()

				var evicted atomic.Bool

				health.registerNodeWS("node-a", func() { evicted.Store(true) })
				health.registerNodeWS("node-b", func() { evicted.Store(true) })

				mux := http.NewServeMux()
				registerPushHandlers(mux, health, proxy, make(chan struct{}, maxConcurrentNodeWS), issuer)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.TLS = clientTLS
					mux.ServeHTTP(w, r)
				}))
				defer server.Close()

				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()

				conn, _, err := websocket.Dial(ctx, server.URL+path, &websocket.DialOptions{HTTPHeader: http.Header{
					"Authorization":         []string{"Bearer " + token},
					"X-Remote-User":         []string{"system:serviceaccount:unbounded-system:unbounded-net-node"},
					nodeIdentityTokenHeader: []string{"service-account-token"},
				}})
				if err != nil {
					t.Fatal(err)
				}

				defer func() {
					if err := conn.CloseNow(); err != nil {
						t.Logf("close websocket: %v", err)
					}
				}()

				data, err := proto.Marshal(tc.message)
				if err != nil {
					t.Fatal(err)
				}

				if tc.corrupt {
					data = append(data, 0xff)
				}

				if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
					t.Fatal(err)
				}

				frameType, reply, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}

				if frameType != websocket.MessageBinary {
					t.Fatal("expected a binary acknowledgment")
				}

				var ack statusproto.NodeStatusAck
				if err := proto.Unmarshal(reply, &ack); err != nil {
					t.Fatal(err)
				}

				assertJSONIdentityCache(t, health, before, tc.reject)

				if !tc.reject {
					if ack.Status != "ok" || !evicted.Load() {
						t.Fatalf("valid frame was not applied and registered: %v", &ack)
					}

					return
				}

				if ack.Status != "resync_required" || ack.Reason == "" || evicted.Load() {
					t.Fatalf("rejected identity mutated connection state or lost its error: %v", &ack)
				}

				if tc.wantClosed {
					if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
						t.Fatal("authorization rejection did not close the connection")
					}

					return
				}

				// Invalid binary payloads retain the existing resync-and-retry behavior.
				valid, err := proto.Marshal(full("node-a", "node-a"))
				if err != nil {
					t.Fatal(err)
				}

				if err := conn.Write(ctx, websocket.MessageBinary, valid); err != nil {
					t.Fatal(err)
				}

				_, reply, err = conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}

				if err := proto.Unmarshal(reply, &ack); err != nil {
					t.Fatal(err)
				}

				if ack.Status != "ok" || !evicted.Load() {
					t.Fatalf("valid retry failed: %v", &ack)
				}

				assertJSONIdentityCache(t, health, before, false)
			})
		}
	}
}
