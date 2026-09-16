// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestSummaryPublicationNeverCollectsDetails(t *testing.T) {
	s := summaryRouteFixture()
	s.bpfCollector = func() []BpfEntry { t.Fatal("routine summary collected BPF"); return nil }
	h := blockedBootstrapHealthState()
	h.setStatusServer(s)

	cfg := &config{StatusDetailMode: "summary", StatusPushDelta: true}
	for _, force := range []bool{true, false} {
		msg, base := collectPublication(h, cfg, &NodeStatusResponse{Peers: make([]WireGuardPeerStatus, 100)}, force, 42)
		if base != nil || msg.Status != nil || msg.Delta != nil || msg.Type != statusv1alpha1.NodeStatusSummaryType || msg.Summary == nil {
			t.Fatalf("summary retained details: %v base=%v", msg, base)
		}

		if len(msg.Summary.NodeErrors) == 0 || msg.BaseRevision != 42 {
			t.Fatal("summary lost guard/revision")
		}
	}
}

func TestDetailACKDoesNotReleasePublication(t *testing.T) {
	state := &statusAckState{}
	state.revision.Store(7)
	state.pending.Store(true)

	for _, ack := range []*statusv1alpha1.NodeStatusAck{
		{Status: statusv1alpha1.DetailRequestStatus, DetailRequest: &statusv1alpha1.DetailRequest{RequestID: "r", Deadline: time.Now().Add(time.Minute)}},
		{Status: "ok", DetailRequestID: "r", Revision: 99, SummarySupported: true},
	} {
		data, err := proto.Marshal(netstatus.NodeStatusAckToProto(ack))
		if err != nil {
			t.Fatal(err)
		}

		if state.accept(data) || !state.pending.Load() || state.revision.Load() != 7 || state.summary.Load() {
			t.Fatal("detail traffic changed publication ACK state")
		}
	}
}

func TestRoutineSummaryPublishers(t *testing.T) {
	for _, transport := range []string{"HTTP", "WS"} {
		for _, supported := range []bool{true, false} {
			t.Run(transport+map[bool]string{true: "/supported", false: "/unsupported"}[supported], func(t *testing.T) {
				messages := make(chan *statusproto.NodeStatusMessage, 32)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handle := func(data []byte) []byte {
						var msg statusproto.NodeStatusMessage
						if err := proto.Unmarshal(data, &msg); err != nil {
							t.Error(err)
						}

						select {
						case messages <- &msg:
						case <-r.Context().Done():
						}

						ack, _ := proto.Marshal(&statusproto.NodeStatusAck{Status: "resync_required", Revision: 3, SummarySupported: supported})

						return ack
					}

					if transport == "WS" {
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

							if err := conn.Write(r.Context(), websocket.MessageBinary, handle(data)); err != nil {
								return
							}
						}
					}

					reader, err := gzip.NewReader(r.Body)
					if err != nil {
						t.Error(err)
						return
					}

					data, err := io.ReadAll(reader)
					_ = reader.Close()

					if err != nil {
						t.Error(err)
						return
					}

					_, _ = w.Write(handle(data))
				}))
				defer server.Close()

				cfg := &config{
					NodeName: "node-a", StatusDetailMode: "summary", StatusPushEnabled: transport == "HTTP",
					StatusWSEnabled: transport == "WS", StatusPushURL: server.URL, StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http"),
					StatusPushInterval: 5 * time.Millisecond, StatusPushDelta: true, StatusWSAPIServerMode: statusWSAPIServerModeNever,
					CriticalDeltaEvery: 5 * time.Millisecond, StatsDeltaEvery: 7 * time.Millisecond, FullSyncEvery: 9 * time.Millisecond,
				}
				h := blockedBootstrapHealthState()
				ctx, cancel := context.WithCancel(t.Context())
				startStatusPublishers(ctx, cfg, h)

				defer func() { cancel(); h.stopStatusPublishers() }()

				want := 3
				if !supported {
					want = 1
				}

				for range want {
					select {
					case msg := <-messages:
						if msg.Type != statusv1alpha1.NodeStatusSummaryType || msg.Summary == nil || msg.Status != nil || msg.Delta != nil {
							t.Fatalf("unexpected routine message: %v", msg)
						}

						if len(msg.Summary.NodeErrors) == 0 || msg.Summary.NodeInfo.Name != "node-a" {
							t.Fatal("lost bootstrap guard/identity")
						}
					case <-time.After(3 * time.Second):
						t.Fatal("no summary received")
					}
				}

				if !supported {
					waitForStatusCondition(t, func() bool {
						for _, err := range h.getSummarySnapshot().NodeErrors {
							if err.Type == nodeErrorSummaryUnsupported {
								return true
							}
						}

						return false
					})
				}
			})
		}
	}
}
