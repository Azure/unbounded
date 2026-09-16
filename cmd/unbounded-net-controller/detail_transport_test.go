// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func encodeDetailTransportMessage(t *testing.T, binary bool, requestID string) []byte {
	t.Helper()

	message := &statusproto.NodeStatusMessage{
		Type: statusv1alpha1.NodeStatusSummaryType, NodeName: "node-a", SupportsDetails: true,
		Summary: &statusproto.NodeStatusOverview{NodeInfo: &statusproto.NodeInfo{Name: "node-a"}, PeerCount: 1},
	}
	jsonMessage := NodeStatusWSMessage{Type: message.Type, NodeName: message.NodeName, SupportsDetails: true}
	overview := protoToNodeOverview(message.Summary)
	jsonMessage.Summary = &overview

	if requestID != "" {
		message.Type = statusv1alpha1.NodeStatusDetailsType
		message.Summary = nil
		message.DetailRequestId = requestID
		message.Status = &statusproto.NodeStatusFull{
			NodeInfo: &statusproto.NodeInfo{Name: "node-a"},
			Peers:    []*statusproto.PeerStatus{{Name: "peer"}},
		}
		full := protoToNodeStatus(message.Status)
		jsonMessage.Type = message.Type
		jsonMessage.Summary = nil
		jsonMessage.Status = &full
		jsonMessage.DetailRequestID = requestID
	}

	var (
		data []byte
		err  error
	)
	if binary {
		data, err = proto.Marshal(message)
	} else {
		data, err = json.Marshal(jsonMessage)
	}

	if err != nil {
		t.Fatal(err)
	}

	return data
}

func decodeDetailTransportAck(t *testing.T, binary, ws bool, data []byte) *NodeStatusPushAck {
	t.Helper()

	if binary {
		var ack statusproto.NodeStatusAck
		if err := proto.Unmarshal(data, &ack); err != nil {
			t.Fatal(err)
		}

		return statuspkg.NodeStatusAckFromProto(&ack)
	}

	var ack NodeStatusPushAck
	if ws {
		var envelope struct {
			Data NodeStatusPushAck `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}

		ack = envelope.Data
	} else if err := json.Unmarshal(data, &ack); err != nil {
		t.Fatal(err)
	}

	return &ack
}

func awaitDetailTransport(t *testing.T, ready func() bool) {
	t.Helper()

	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()

	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for !ready() {
		select {
		case <-timeout.C:
			t.Fatal("detail transport did not become ready")
		case <-tick.C:
		}
	}
}

func TestDetailWebSocketCommandAndResponse(t *testing.T) {
	for _, binary := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "protobuf"}[binary], func(t *testing.T) {
			health := newJSONIdentityHealth()
			health.registerAggregatedAPIServer = false
			manager := testDetailRequests(t, nodeDetailRequestHooks{
				Dispatch: health.dispatchNodeDetail,
				Pull: func(context.Context, string) (*NodeStatusResponse, error) {
					t.Error("active WebSocket request unexpectedly used HTTP")
					return nil, nil
				},
			})
			health.detailRequests = manager
			issuer := testTokenIssuer(t)
			mux := http.NewServeMux()
			registerPushHandlers(mux, health, nil, make(chan struct{}, 1), issuer)

			server := httptest.NewServer(mux)
			defer server.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			conn, _, err := websocket.Dial(ctx, server.URL+"/status/nodews", &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": {"Bearer " + testNodeToken(t, issuer)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()

			frameType := websocket.MessageText
			if binary {
				frameType = websocket.MessageBinary
			}

			send := func(id string) {
				t.Helper()

				if err := conn.Write(ctx, frameType, encodeDetailTransportMessage(t, binary, id)); err != nil {
					t.Fatal(err)
				}
			}
			read := func() *NodeStatusPushAck {
				t.Helper()

				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}

				return decodeDetailTransportAck(t, binary, true, data)
			}

			send("")

			publication := read()

			awaitDetailTransport(t, func() bool {
				health.nodeWSMu.Lock()
				defer health.nodeWSMu.Unlock()

				return health.nodeWSRegistry["node-a"] != nil && health.nodeWSRegistry["node-a"].send != nil
			})

			request := manager.Request("node-a", true)

			command := read()
			if command.IsPublicationAck() || command.DetailRequest == nil ||
				command.DetailRequest.RequestID != request.RequestID || !command.SummarySupported {
				t.Fatalf("wire command was not distinct from an ordinary ACK: %+v", command)
			}

			send(request.RequestID)

			ack := read()
			if ack.Status != "ok" || ack.DetailRequestID != request.RequestID || ack.Revision != 0 || ack.IsPublicationAck() {
				t.Fatalf("invalid correlated ACK: %+v", ack)
			}

			result := manager.Result("node-a", request.RequestID)
			if result.Details == nil || len(result.Details.Status.Peers) != 1 {
				t.Fatal("one-shot details did not complete the request")
			}

			cached, _ := health.statusCache.Get("node-a")
			if cached.Revision != publication.Revision || cached.Status.Peers != nil {
				t.Fatal("one-shot reply became the routine delta base")
			}

			expires := result.Details.ExpiresAt

			send(request.RequestID)

			if duplicate := read(); duplicate.Status != "ok" {
				t.Fatal("duplicate detail reply was not idempotent")
			}

			if !manager.Result("node-a", request.RequestID).Details.ExpiresAt.Equal(expires) {
				t.Fatal("duplicate detail reply renewed TTL")
			}
		})
	}
}

func TestDetailHTTPPollingCommandAndResponse(t *testing.T) {
	for _, binary := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "protobuf"}[binary], func(t *testing.T) {
			health := newJSONIdentityHealth()
			health.registerAggregatedAPIServer = false
			manager := testDetailRequests(t, nodeDetailRequestHooks{})
			health.detailRequests = manager
			issuer := testTokenIssuer(t)
			token := testNodeToken(t, issuer)
			mux := http.NewServeMux()
			registerPushHandlers(mux, health, nil, make(chan struct{}, 1), issuer)

			request := manager.Request("node-a", true)

			awaitDetailTransport(t, func() bool { _, ok := manager.Pending("node-a"); return ok })

			post := func(id string) *NodeStatusPushAck {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, "/status/push", bytes.NewReader(encodeDetailTransportMessage(t, binary, id)))
				r.Header.Set("Authorization", "Bearer "+token)

				if binary {
					r.Header.Set("Content-Type", "application/x-protobuf")
				}

				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)

				if w.Code != http.StatusOK {
					t.Fatalf("POST failed: %d %s", w.Code, w.Body.String())
				}

				return decodeDetailTransportAck(t, binary, false, w.Body.Bytes())
			}

			publication := post("")
			if !publication.SummarySupported || !publication.IsPublicationAck() ||
				publication.DetailRequest == nil || publication.DetailRequest.RequestID != request.RequestID {
				t.Fatalf("POST ACK lost capabilities or polling command: %+v", publication)
			}

			detailAck := post(request.RequestID)
			if detailAck.Status != "ok" || detailAck.DetailRequestID != request.RequestID || detailAck.IsPublicationAck() || detailAck.DetailRequest != nil {
				t.Fatalf("POST detail ACK corrupted publication/polling state: %+v", detailAck)
			}

			cached, _ := health.statusCache.Get("node-a")
			if cached.Revision != publication.Revision || manager.Result("node-a", request.RequestID).Details == nil {
				t.Fatal("POST detail response changed routine state or failed to complete")
			}
		})
	}
}

func TestDetailFailureKeepsPreviousValidSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})

		first := manager.Request("node", true)
		if err := manager.Complete("node", first.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		before, _ := manager.cache.Get("node")

		refresh := manager.Request("node", true)
		if err := manager.Fail("other", refresh.RequestID, "bad"); err == nil {
			t.Fatal("failure for a different node was accepted")
		}

		if err := manager.Fail("node", refresh.RequestID, "response exceeds the transport frame limit"); err != nil {
			t.Fatal(err)
		}

		result := manager.Result("node", refresh.RequestID)
		if result.State != statusv1alpha1.NodeDetailUnavailable || result.Error == "" || result.Details != nil {
			t.Fatal("collection failure was not explicit")
		}

		after, ok := manager.cache.Get("node")
		if !ok || after.Status != before.Status || !after.ExpiresAt.Equal(before.ExpiresAt) {
			t.Fatal("failed refresh destroyed or renewed the old valid snapshot")
		}
	})
}
