// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func decodeTestHTTPStatus(t *testing.T, body io.Reader) *statusproto.NodeStatusMessage {
	t.Helper()

	reader, err := gzip.NewReader(body)
	if err != nil {
		t.Error(err)
		return nil
	}

	data, err := io.ReadAll(reader)
	_ = reader.Close()

	if err != nil {
		t.Error(err)
		return nil
	}

	var msg statusproto.NodeStatusMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Error(err)
		return nil
	}

	return &msg
}

func TestHTTPDetailsWakeImmediatelyAndPreserveBase(t *testing.T) {
	t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	for _, mode := range []string{"summary", "full"} {
		for _, fallback := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "/fallback", false: "/direct"}[fallback], func(t *testing.T) {
				messages := make(chan *statusproto.NodeStatusMessage, 8)

				var detailCount atomic.Int32

				deadline := time.Now().Add(time.Minute)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if fallback && r.URL.Path == "/direct" {
						http.Error(w, "direct unavailable", http.StatusServiceUnavailable)
						return
					}

					if r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Content-Encoding") != "gzip" {
						t.Error("detail path changed status HTTP encoding")
					}

					msg := decodeTestHTTPStatus(t, r.Body)
					if msg == nil {
						return
					}

					select {
					case messages <- msg:
					case <-r.Context().Done():
						return
					}

					ack := statusv1alpha1.NodeStatusAck{
						Status: "ok", Revision: 7, SummarySupported: true,
						DetailRequest: &statusv1alpha1.DetailRequest{RequestID: "request", Deadline: deadline},
					}

					if msg.Type == statusv1alpha1.NodeStatusDetailsType {
						detailCount.Add(1)

						ack.DetailRequestID, ack.Revision = "request", 99
					}
					// Shared JSON ACKs must carry the same commands as protobuf.
					if err := json.NewEncoder(w).Encode(ack); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()

				cfg := &config{
					NodeName: "node-a", StatusDetailMode: mode, StatusPushEnabled: true, StatusPushDelta: true,
					StatusPushURL: server.URL + "/direct", StatusPushInterval: time.Second,
					StatusPushAPIServerInterval: time.Hour, StatusWSAPIServerMode: statusWSAPIServerModeFallback,
					StatusWSAPIServerURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/apis/status/nodews",
				}
				h := blockedBootstrapHealthState()
				ctx, cancel := context.WithCancel(t.Context())
				startStatusPublishers(ctx, cfg, h)

				defer func() { cancel(); h.stopStatusPublishers() }()

				select {
				case msg := <-messages:
					if !msg.SupportsDetails || msg.Type == statusv1alpha1.NodeStatusDetailsType {
						t.Fatalf("bad initial publication: %v", msg)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("no initial publication")
				}

				select {
				case msg := <-messages:
					if msg.Type != statusv1alpha1.NodeStatusDetailsType || msg.DetailRequestId != "request" || msg.Status == nil || msg.BaseRevision != 0 {
						t.Fatalf("bad immediate detail response: %v", msg)
					}
				case <-time.After(400 * time.Millisecond):
					t.Fatal("detail response waited for routine or API fallback interval")
				}

				if !fallback {
					select {
					case msg := <-messages:
						if msg.Type == statusv1alpha1.NodeStatusDetailsType || msg.BaseRevision != 7 {
							t.Fatalf("detail ACK changed publication base or duplicate recollected: %v", msg)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("no subsequent publication")
					}
				}

				waitForStatusCondition(t, func() bool {
					state := h.detailState()
					state.mu.Lock()
					defer state.mu.Unlock()

					reply := state.replies["request"]

					return reply != nil && reply.done && reply.payload == nil
				})

				if detailCount.Load() != 1 {
					t.Fatalf("duplicate command generated %d replies", detailCount.Load())
				}
			})
		}
	}
}

func TestHTTPDetailRetryDoesNotRecollectOrWaitForRoutineTick(t *testing.T) {
	h := blockedBootstrapHealthState()
	state := h.detailState()

	req := &statusv1alpha1.DetailRequest{RequestID: "retry", Deadline: time.Now().Add(time.Minute)}
	if err := state.enqueue(req, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Seed the immutable reply as though a previous channel disconnected.
	first := state.take("node-a", h.getStatusSnapshot, time.Now())
	state.finish(first.id)

	requests := make(chan *statusproto.NodeStatusMessage, 4)

	var count atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := decodeTestHTTPStatus(t, r.Body)
		requests <- msg

		if count.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}

		payload, err := proto.Marshal(&statusproto.NodeStatusAck{Status: "ok", DetailRequestId: "retry", Revision: 999})
		if err != nil {
			t.Error(err)
			return
		}

		_, _ = w.Write(payload)
	}))
	defer server.Close()

	cfg := &config{
		NodeName: "node-a", StatusDetailMode: "summary", StatusPushEnabled: true,
		StatusPushURL: server.URL, StatusPushInterval: time.Hour, StatusWSAPIServerMode: statusWSAPIServerModeNever,
	}
	ctx, cancel := context.WithCancel(t.Context())
	startStatusPublishers(ctx, cfg, h)

	defer func() { cancel(); h.stopStatusPublishers() }()

	var original statusproto.NodeStatusMessage
	if err := proto.Unmarshal(first.payload, &original); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		select {
		case msg := <-requests:
			if !proto.Equal(&original, msg) {
				t.Fatal("HTTP retry recollected or changed the response")
			}
		case <-time.After(4 * time.Second):
			t.Fatal("HTTP detail retry waited for routine publication")
		}
	}
}

func TestHTTPDetailCompressedLimitProducesRetriableError(t *testing.T) {
	random := make([]byte, nodeDetailHTTPBodyLimit+64*1024)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}

	state := (&nodeHealthState{}).detailState()

	req := &statusv1alpha1.DetailRequest{RequestID: "large", Deadline: time.Now().Add(time.Minute)}
	if err := state.enqueue(req, time.Now()); err != nil {
		t.Fatal(err)
	}

	collections := 0

	delivery := state.take("node", func() *NodeStatusResponse {
		collections++
		return &NodeStatusResponse{NodeErrors: []NodeError{{Message: base64.StdEncoding.EncodeToString(random)}}}
	}, time.Now())
	if delivery == nil {
		t.Fatal("no detail delivery")
	}

	body, err := state.httpBody("node", delivery, delivery.payload)
	if err != nil {
		t.Fatal(err)
	}

	msg := decodeTestHTTPStatus(t, bytes.NewReader(body))
	if msg == nil || msg.Status != nil || !strings.Contains(msg.DetailError, "1 MiB") || msg.DetailRequestId != "large" {
		t.Fatalf("oversized detail did not produce correlated failure: %v", msg)
	}

	state.finish(delivery.id)

	retry := state.take("node", func() *NodeStatusResponse { t.Fatal("oversized retry recollected"); return nil }, time.Now().Add(2*time.Second))
	if retry == nil || !bytes.Equal(retry.payload, delivery.payload) || collections != 1 {
		t.Fatal("oversized payload retained or changed on retry")
	}
}
