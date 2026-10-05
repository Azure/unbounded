// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	configpkg "github.com/Azure/unbounded/internal/net/config"
	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestSummaryPublicationNeverCollectsDetails(t *testing.T) {
	s := summaryRouteFixture()
	s.bpfCollector = func() []BpfEntry { t.Fatal("routine summary collected BPF"); return nil }
	h := blockedBootstrapHealthState()
	h.setStatusServer(s)

	cfg := &config{StatusDetailMode: configpkg.DefaultStatusDetailMode, StatusPushDelta: true}
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

func TestSummaryPublicationFiltersTransportErrorsAcrossHTTPAndWebSocket(t *testing.T) {
	h := blockedBootstrapHealthState()
	h.transientErrors = []NodeError{
		{Type: nodeErrorTypeDirectPush, Message: "direct push unavailable"},
		{Type: nodeErrorTypeDirectWebSocket, Message: "direct websocket unavailable"},
		{Type: "cni", Message: "CNI reconciliation failed"},
	}

	httpMessage, _ := collectPublication(h, &config{StatusDetailMode: "summary"}, nil, false, 42)

	wsPayload, err := marshalStatusWebSocketSummary(publicationSummary(h), 42)
	if err != nil {
		t.Fatal(err)
	}

	var wsMessage statusproto.NodeStatusMessage
	if err := proto.Unmarshal(wsPayload, &wsMessage); err != nil {
		t.Fatal(err)
	}

	httpMessage.Summary.TimestampUnixNs = 0

	wsMessage.Summary.TimestampUnixNs = 0
	if !proto.Equal(httpMessage.Summary, wsMessage.Summary) {
		t.Fatalf("HTTP and WebSocket summaries differ: HTTP=%v WS=%v", httpMessage.Summary, wsMessage.Summary)
	}

	errorTypes := make(map[string]bool)
	for _, nodeError := range httpMessage.Summary.NodeErrors {
		errorTypes[nodeError.Type] = true
	}

	if errorTypes[nodeErrorTypeDirectPush] || errorTypes[nodeErrorTypeDirectWebSocket] {
		t.Fatalf("summary retained local transport diagnostics: %v", httpMessage.Summary.NodeErrors)
	}

	if !errorTypes["cni"] || !errorTypes[configPodCIDRGuard] {
		t.Fatalf("summary lost actual node errors: %v", httpMessage.Summary.NodeErrors)
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

		data, err = json.Marshal(ack)
		if err != nil {
			t.Fatal(err)
		}

		if state.accept(data) || !state.pending.Load() || state.revision.Load() != 7 || state.summary.Load() {
			t.Fatal("JSON detail traffic changed publication ACK state")
		}
	}
}

func TestSlowDetailCollectionDoesNotBlockRoutinePublishers(t *testing.T) {
	for _, transport := range []string{"HTTP", "WS"} {
		t.Run(transport, func(t *testing.T) {
			messages := make(chan *statusproto.NodeStatusMessage, 8)
			collectionStarted := make(chan struct{})
			releaseCollection := make(chan struct{})

			var requestOnce sync.Once

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handle := func(data []byte) []byte {
					var msg statusproto.NodeStatusMessage
					if err := proto.Unmarshal(data, &msg); err != nil {
						t.Error(err)
					}

					messages <- &msg

					ack := &statusproto.NodeStatusAck{Status: "ok", Revision: 1, SummarySupported: true}

					requestOnce.Do(func() {
						ack.DetailRequest = &statusproto.DetailRequest{
							RequestId:      "slow",
							DeadlineUnixNs: time.Now().Add(time.Minute).UnixNano(),
						}
					})

					payload, err := proto.Marshal(ack)
					if err != nil {
						t.Error(err)
					}

					return payload
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

				var ack statusproto.NodeStatusAck
				if err := proto.Unmarshal(handle(data), &ack); err != nil {
					t.Error(err)

					return
				}

				if err := json.NewEncoder(w).Encode(netstatus.NodeStatusAckFromProto(&ack)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			statusServer := summaryRouteFixture()
			statusServer.bpfCollector = func() []BpfEntry {
				close(collectionStarted)
				<-releaseCollection

				return nil
			}

			health := blockedBootstrapHealthState()
			health.setStatusServer(statusServer)

			cfg := &config{
				NodeName: "node-a", StatusDetailMode: "summary", StatusPushEnabled: transport == "HTTP",
				StatusWSEnabled: transport == "WS", StatusPushURL: server.URL, StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http"),
				StatusPushInterval: 10 * time.Millisecond, StatusWSAPIServerMode: statusWSAPIServerModeNever,
				CriticalDeltaEvery: 10 * time.Millisecond, StatsDeltaEvery: time.Hour, FullSyncEvery: time.Hour,
			}
			ctx, cancel := context.WithCancel(t.Context())
			startStatusPublishers(ctx, cfg, health)

			defer func() {
				close(releaseCollection)
				cancel()
				health.stopStatusPublishers()
			}()

			select {
			case <-messages:
			case <-time.After(3 * time.Second):
				t.Fatal("no initial summary publication")
			}

			select {
			case <-collectionStarted:
			case <-time.After(time.Second):
				t.Fatal("detail collection did not start")
			}

			health.setCNIReady("cbr0", []string{"10.244.7.0/24"})

			select {
			case msg := <-messages:
				if msg.Type != statusv1alpha1.NodeStatusSummaryType || msg.Summary == nil {
					t.Fatalf("routine publication changed while detail collection was blocked: %v", msg)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("slow detail collection blocked routine publication")
			}
		})
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
					NodeName: "node-a", StatusDetailMode: configpkg.DefaultStatusDetailMode, StatusPushEnabled: transport == "HTTP",
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

				if supported {
					h.setCNIReady("cbr0", []string{"10.244.7.0/24"})

					recovered := false
					timeout := time.After(3 * time.Second)

					for !recovered {
						select {
						case msg := <-messages:
							if msg.Summary == nil || msg.Status != nil || msg.Delta != nil {
								t.Fatalf("recovery published details: %v", msg)
							}

							recovered = true

							for _, nodeError := range msg.Summary.NodeErrors {
								if nodeError.Type == configPodCIDRGuard {
									recovered = false
								}
							}
						case <-timeout:
							t.Fatal("summary did not publish CNI recovery")
						}
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
