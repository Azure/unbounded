// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func sendTestStatusAck(ctx context.Context, conn *websocket.Conn, ack *statusproto.NodeStatusAck) error {
	payload, err := proto.Marshal(ack)
	if err != nil {
		return err
	}

	return conn.Write(ctx, websocket.MessageBinary, payload)
}

func TestWebSocketDetailsWakeWhilePublicationPending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		oversized bool
	}{
		{name: "summary", mode: "summary"},
		{name: "full", mode: "full"},
		{name: "oversized", mode: "summary", oversized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := make(chan *statusproto.NodeStatusMessage, 8)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close(websocket.StatusNormalClosure, "done") }()

				for {
					_, data, err := conn.Read(r.Context())
					if err != nil {
						return
					}

					var msg statusproto.NodeStatusMessage
					if err := proto.Unmarshal(data, &msg); err != nil {
						t.Error(err)
						return
					}

					select {
					case messages <- &msg:
					case <-r.Context().Done():
						return
					}

					if msg.Type == statusv1alpha1.NodeStatusDetailsType {
						if err := sendTestStatusAck(r.Context(), conn, &statusproto.NodeStatusAck{
							Status: "ok", DetailRequestId: "request", Revision: 99,
						}); err != nil {
							return
						}
					} else {
						if err := sendTestStatusAck(r.Context(), conn, &statusproto.NodeStatusAck{
							Status:        statusv1alpha1.DetailRequestStatus,
							DetailRequest: &statusproto.DetailRequest{RequestId: "expired", DeadlineUnixNs: time.Now().Add(-time.Second).UnixNano()},
						}); err != nil {
							return
						}
					}
					// No publication ACK is sent. This command (and its delayed
					// duplicate after the detail ACK) cannot release that pending ACK.
					if err := sendTestStatusAck(r.Context(), conn, &statusproto.NodeStatusAck{
						Status:        statusv1alpha1.DetailRequestStatus,
						DetailRequest: &statusproto.DetailRequest{RequestId: "request", DeadlineUnixNs: time.Now().Add(time.Minute).UnixNano()},
					}); err != nil {
						return
					}
				}
			}))
			defer server.Close()

			s := summaryRouteFixture()
			s.cfg.NodeName = "node-a"

			var collections atomic.Int32

			s.bpfCollector = func() []BpfEntry {
				collections.Add(1)

				if tc.oversized {
					return []BpfEntry{{CIDR: strings.Repeat("x", nodeDetailFrameLimit)}}
				}

				return []BpfEntry{{CIDR: "10.0.0.0/8"}}
			}
			h := blockedBootstrapHealthState()
			h.setStatusServer(s)

			cfg := &config{
				NodeName: "node-a", StatusDetailMode: tc.mode, StatusWSEnabled: true,
				StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http"), StatusWSAPIServerMode: statusWSAPIServerModeNever,
				CriticalDeltaEvery: time.Hour, StatsDeltaEvery: time.Hour, FullSyncEvery: time.Hour,
			}
			ctx, cancel := context.WithCancel(t.Context())
			startStatusPublishers(ctx, cfg, h)

			defer func() { cancel(); h.stopStatusPublishers() }()

			for i := range 2 {
				select {
				case msg := <-messages:
					if !msg.SupportsDetails {
						t.Fatal("missing responder capability")
					}

					if i == 1 && (msg.Type != statusv1alpha1.NodeStatusDetailsType || msg.DetailRequestId != "request" ||
						msg.BaseRevision != 0) {
						t.Fatalf("invalid immediate detail reply: %v", msg)
					}

					if i == 1 {
						if tc.oversized {
							if msg.Status != nil || !strings.Contains(msg.DetailError, "WebSocket frame limit") {
								t.Fatalf("oversized websocket details were not rejected: %v", msg)
							}
						} else if msg.Status == nil || len(msg.Status.BpfEntries) != 1 || msg.DetailError != "" {
							t.Fatalf("detail response lost payload: %v", msg)
						}
					}
				case <-time.After(3 * time.Second):
					t.Fatal("detail command waited for a periodic publication tick")
				}
			}

			waitForStatusCondition(t, func() bool {
				state := h.detailState()
				state.mu.Lock()
				defer state.mu.Unlock()

				reply := state.replies["request"]

				return reply != nil && reply.done && reply.payload == nil
			})

			select {
			case msg := <-messages:
				t.Fatalf("duplicate or expired command produced another reply: %v", msg)
			case <-time.After(100 * time.Millisecond):
			}

			want := int32(1)
			if tc.mode == "full" {
				want++
			}

			if collections.Load() != want {
				t.Fatalf("collected %d full snapshots, want %d", collections.Load(), want)
			}
		})
	}
}

func TestWebSocketDetailRetrySurvivesReconnect(t *testing.T) {
	deliveries := make(chan []byte, 4)

	var connections atomic.Int32

	deadline := time.Now().Add(time.Minute)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}

		number := connections.Add(1)

		defer func() { _ = conn.Close(websocket.StatusGoingAway, "reconnect") }()

		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}

			var msg statusproto.NodeStatusMessage
			if err := proto.Unmarshal(data, &msg); err != nil {
				t.Error(err)
				return
			}

			if msg.Type == statusv1alpha1.NodeStatusDetailsType {
				deliveries <- data

				if number == 1 {
					return
				}

				if err := sendTestStatusAck(r.Context(), conn, &statusproto.NodeStatusAck{Status: "ok", DetailRequestId: "retry"}); err != nil {
					return
				}
			} else if err := sendTestStatusAck(r.Context(), conn, &statusproto.NodeStatusAck{
				Status: "ok", Revision: 1, SummarySupported: true,
				DetailRequest: &statusproto.DetailRequest{RequestId: "retry", DeadlineUnixNs: deadline.UnixNano()},
			}); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	h := blockedBootstrapHealthState()
	s := summaryRouteFixture()
	s.cfg.NodeName = "node-a"

	var collections atomic.Int32

	s.bpfCollector = func() []BpfEntry { collections.Add(1); return nil }
	h.setStatusServer(s)

	cfg := &config{
		NodeName: "node-a", StatusDetailMode: "summary", StatusWSEnabled: true,
		StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http"), StatusWSAPIServerMode: statusWSAPIServerModeNever,
		CriticalDeltaEvery: time.Hour, StatsDeltaEvery: time.Hour, FullSyncEvery: time.Hour,
	}
	ctx, cancel := context.WithCancel(t.Context())
	startStatusPublishers(ctx, cfg, h)

	defer func() { cancel(); h.stopStatusPublishers() }()

	var first []byte

	for range 2 {
		select {
		case data := <-deliveries:
			if first != nil && string(first) != string(data) {
				t.Fatal("reconnect changed the collected reply")
			}

			first = data
		case <-time.After(4 * time.Second):
			t.Fatal("outstanding detail was not retried after reconnect")
		}
	}

	if collections.Load() != 1 {
		t.Fatal("reconnect recollected details")
	}
}
