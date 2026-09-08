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
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func blockedBootstrapHealthState() *nodeHealthState {
	health := &nodeHealthState{}
	health.setBootstrapSnapshot("node-a", "site-a", "pub-a", []string{"10.244.7.0/24"}, false)
	health.beginManagedCNI("cbr0")
	health.setCNIBlocked("CNI configuration blocked; remaining unready bridge=cbr0 assignedPodCIDRs=[10.244.7.0/24] interface=veth-old address=10.244.6.9 reason=address outside assigned PodCIDRs")

	return health
}

func assertBootstrapGuardStatus(t *testing.T, message *statusproto.NodeStatusMessage) {
	t.Helper()

	if message.GetStatus() == nil {
		t.Fatalf("expected full bootstrap status, got %#v", message)
	}

	if message.GetNodeName() != "node-a" || message.GetStatus().GetNodeInfo().GetSiteName() != "site-a" {
		t.Fatalf("unexpected bootstrap identity: %#v", message.GetStatus().GetNodeInfo())
	}

	if got := message.GetStatus().GetNodeInfo().GetPodCidrs(); len(got) != 1 || got[0] != "10.244.7.0/24" {
		t.Fatalf("unexpected bootstrap PodCIDRs: %v", got)
	}

	errors := message.GetStatus().GetNodeErrors()
	if len(errors) != 1 || errors[0].GetType() != configPodCIDRGuard ||
		!strings.Contains(errors[0].GetMessage(), "veth-old") {
		t.Fatalf("unexpected bootstrap node errors: %#v", errors)
	}
}

func isCNIGuardClearingDelta(message *statusproto.NodeStatusMessage) bool {
	if message.GetType() != "node_status_delta" || message.GetDelta() == nil {
		return false
	}

	for _, field := range message.GetDelta().GetUpdatedFields() {
		if field == "nodeErrors" {
			return len(message.GetDelta().GetNodeErrors()) == 0
		}
	}

	return false
}

func TestHTTPPublisherReportsBootstrapCNIGuard(t *testing.T) {
	received := make(chan *statusproto.NodeStatusMessage, 16)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("create gzip reader: %v", err)
			http.Error(w, "bad gzip", http.StatusBadRequest)

			return
		}

		data, err := io.ReadAll(reader)
		_ = reader.Close()

		if err != nil {
			t.Errorf("read request: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)

			return
		}

		var message statusproto.NodeStatusMessage
		if err := proto.Unmarshal(data, &message); err != nil {
			t.Errorf("unmarshal status: %v", err)
			http.Error(w, "bad protobuf", http.StatusBadRequest)

			return
		}

		select {
		case received <- &message:
		default:
		}

		ack, _ := proto.Marshal(&statusproto.NodeStatusAck{Status: "ok", Revision: 1})

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(ack)
	}))
	defer server.Close()

	cfg := &config{
		NodeName:                    "node-a",
		StatusPushEnabled:           true,
		StatusPushURL:               server.URL,
		StatusPushInterval:          10 * time.Millisecond,
		StatusPushAPIServerInterval: time.Hour,
		StatusWSAPIServerMode:       statusWSAPIServerModeNever,
		StatusPushDelta:             true,
	}
	health := blockedBootstrapHealthState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startStatusPublishers(ctx, cfg, health)
	defer health.stopStatusPublishers()

	select {
	case message := <-received:
		assertBootstrapGuardStatus(t, message)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for bootstrap HTTP status")
	}

	if ready, _ := health.cniReadiness(); ready {
		t.Fatal("successful HTTP transport must not clear CNI readiness block")
	}

	health.setCNIReady("cbr0", []string{"10.244.7.0/24"})

	deadline := time.After(3 * time.Second)

	for {
		select {
		case message := <-received:
			if isCNIGuardClearingDelta(message) {
				cancel()

				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for HTTP recovery delta to clear CNI guard")
		}
	}
}

func TestWebSocketPublisherReportsBootstrapCNIGuard(t *testing.T) {
	received := make(chan *statusproto.NodeStatusMessage, 16)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)

			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "test complete") }()

		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}

			var message statusproto.NodeStatusMessage
			if err := proto.Unmarshal(data, &message); err != nil {
				t.Errorf("unmarshal websocket status: %v", err)

				return
			}

			received <- &message

			ack, _ := proto.Marshal(&statusproto.NodeStatusAck{Status: "ok", Revision: 1})
			if err := conn.Write(r.Context(), websocket.MessageBinary, ack); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config{
		NodeName:                  "node-a",
		StatusWSEnabled:           true,
		StatusWSURL:               "ws" + strings.TrimPrefix(server.URL, "http"),
		StatusWSAPIServerMode:     statusWSAPIServerModeNever,
		CriticalDeltaEvery:        10 * time.Millisecond,
		StatsDeltaEvery:           time.Hour,
		FullSyncEvery:             time.Hour,
		StatusWSKeepaliveInterval: 0,
	}
	health := blockedBootstrapHealthState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startStatusPublishers(ctx, cfg, health)
	defer health.stopStatusPublishers()

	select {
	case message := <-received:
		assertBootstrapGuardStatus(t, message)
	case <-time.After(3 * time.Second):
		cancel()
		health.stopStatusPublishers()
		t.Fatal("timed out waiting for bootstrap websocket status")
	}

	if ready, _ := health.cniReadiness(); ready {
		t.Fatal("successful websocket transport must not clear CNI readiness block")
	}

	health.setCNIReady("cbr0", []string{"10.244.7.0/24"})

	deadline := time.After(3 * time.Second)

	for {
		select {
		case message := <-received:
			if isCNIGuardClearingDelta(message) {
				cancel()
				health.stopStatusPublishers()

				return
			}
		case <-deadline:
			cancel()
			health.stopStatusPublishers()
			t.Fatal("timed out waiting for websocket recovery delta to clear CNI guard")
		}
	}
}

func TestStatusPublishersJoinInflightHTTPRequest(t *testing.T) {
	started := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}

		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	cfg := &config{
		NodeName:              "node-a",
		StatusPushEnabled:     true,
		StatusPushURL:         server.URL,
		StatusPushInterval:    time.Millisecond,
		StatusWSAPIServerMode: statusWSAPIServerModeNever,
	}
	health := blockedBootstrapHealthState()

	startStatusPublishers(context.Background(), cfg, health)
	defer health.stopStatusPublishers()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not start")
	}

	done := make(chan struct{})

	go func() {
		health.stopStatusPublishers()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel and join in-flight HTTP request")
	}

	health.mu.RLock()
	defer health.mu.RUnlock()

	if len(health.transientErrors) == 0 || !strings.Contains(health.transientErrors[0].Message, "context canceled") {
		t.Fatalf("publisher returned before its canceled request finished: %+v", health.transientErrors)
	}
}

func TestStatusPublishersHonorDisabledTogglesAndJoin(t *testing.T) {
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	cfg := &config{
		NodeName:              "node-a",
		StatusPushEnabled:     false,
		StatusPushURL:         server.URL,
		StatusPushInterval:    time.Millisecond,
		StatusWSEnabled:       false,
		StatusWSURL:           "ws" + strings.TrimPrefix(server.URL, "http"),
		StatusWSAPIServerMode: statusWSAPIServerModeNever,
	}

	health := blockedBootstrapHealthState()
	startStatusPublishers(context.Background(), cfg, health)

	done := make(chan struct{})

	go func() {
		health.stopStatusPublishers()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled status publishers did not join")
	}

	if requests.Load() != 0 {
		t.Fatalf("disabled transports made %d outbound requests", requests.Load())
	}
}
