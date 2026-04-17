// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestWSBroadcasterBasicHelpers tests WSBroadcasterBasicHelpers.
func TestWSBroadcasterBasicHelpers(t *testing.T) {
	b := NewWSBroadcaster(nil)
	if b == nil {
		t.Fatalf("expected broadcaster instance")
	}

	if b.ClientCount() != 0 {
		t.Fatalf("expected zero clients on new broadcaster")
	}

	if b.getSeq() != 0 {
		t.Fatalf("expected initial sequence to be zero")
	}

	client := &WSClient{send: make(chan []byte, 1)}
	b.Register(client)

	if b.ClientCount() != 1 {
		t.Fatalf("expected one client after register")
	}

	b.Unregister(client)

	if b.ClientCount() != 0 {
		t.Fatalf("expected zero clients after unregister")
	}

	if _, ok := <-client.send; ok {
		t.Fatalf("expected unregister to close client send channel")
	}
}

// TestWSBroadcasterNotifyCoalesces tests WSBroadcasterNotifyCoalesces.
func TestWSBroadcasterNotifyCoalesces(t *testing.T) {
	b := NewWSBroadcaster(nil)

	b.Notify()
	b.Notify()

	if got := len(b.notify); got != 1 {
		t.Fatalf("expected notify channel to coalesce to len=1, got %d", got)
	}

	<-b.notify

	if got := len(b.notify); got != 0 {
		t.Fatalf("expected notify channel to be empty after read, got %d", got)
	}
}

// TestWSBroadcasterSendToClient tests WSBroadcasterSendToClient.
func TestWSBroadcasterSendToClient(t *testing.T) {
	b := NewWSBroadcaster(nil)
	client := &WSClient{send: make(chan []byte, 1)}

	b.sendToClient(client, WSMessage{Type: "cluster_status", Message: "ok"})

	select {
	case payload := <-client.send:
		var msg WSMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Fatalf("failed to unmarshal websocket message: %v", err)
		}

		if msg.Type != "cluster_status" || msg.Message != "ok" {
			t.Fatalf("unexpected websocket message: %#v", msg)
		}
	default:
		t.Fatalf("expected message to be written to client channel")
	}

	// Fill the buffer and verify non-blocking drop behavior.
	client.send <- []byte("busy")

	b.sendToClient(client, WSMessage{Type: "cluster_status_delta", Message: "drop-if-full"})

	if got := len(client.send); got != 1 {
		t.Fatalf("expected full buffer length to remain 1, got %d", got)
	}
}

// TestWSBroadcasterRunStopsOnContextCancel tests WSBroadcasterRunStopsOnContextCancel.
func TestWSBroadcasterRunStopsOnContextCancel(t *testing.T) {
	b := NewWSBroadcaster(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		b.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("broadcaster Run did not stop after context cancel")
	}
}

// TestWSBroadcasterBroadcastUpdatePaths tests WSBroadcasterBroadcastUpdatePaths.
func TestWSBroadcasterBroadcastUpdatePaths(t *testing.T) {
	health := &healthState{
		clientset:      k8sfake.NewClientset(),
		statusCache:    NewNodeStatusCache(),
		staleThreshold: time.Minute,
	}
	// Set up a pre-built cluster status cache so broadcastUpdate has data.
	cache := NewClusterStatusCache(health)
	cache.Rebuild(context.Background())
	health.clusterStatusCache = cache

	b := NewWSBroadcaster(health)

	b.broadcastUpdate(context.Background())

	if b.getSeq() != 1 {
		t.Fatalf("expected seq to increment after first broadcast, got %d", b.getSeq())
	}

	b.broadcastUpdate(context.Background())

	if b.getSeq() != 2 {
		t.Fatalf("expected seq to increment after second broadcast, got %d", b.getSeq())
	}
}
