// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func startTestDetailWorker(t *testing.T, state *nodeDetailState, nodeName string, collect func() *NodeStatusResponse) <-chan struct{} {
	t.Helper()

	done := state.start(t.Context(), nodeName, collect)
	t.Cleanup(state.stop)

	return done
}

func waitForDetailDelivery(t *testing.T, state *nodeDetailState) *nodeDetailDelivery {
	t.Helper()

	deadline := time.After(time.Second)

	for {
		if delivery := state.take(time.Now()); delivery != nil {
			return delivery
		}

		select {
		case <-state.wsWake:
		case <-deadline:
			t.Fatal("timed out waiting for detail collection")
		}
	}
}

func TestDetailStateCoalescesRetriesAndReleases(t *testing.T) {
	h := blockedBootstrapHealthState()
	state := h.detailState()
	now := time.Now()

	req := &statusv1alpha1.DetailRequest{RequestID: "one", Deadline: now.Add(time.Minute)}
	if err := state.enqueue(req, now); err != nil {
		t.Fatal(err)
	}

	originalDeadline := req.Deadline

	req.Deadline = req.Deadline.Add(time.Hour)
	if err := state.enqueue(req, now); err != nil {
		t.Fatal(err)
	}

	var count atomic.Int32

	startTestDetailWorker(t, state, "node-a", func() *NodeStatusResponse {
		count.Add(1)

		return h.getStatusSnapshot()
	})

	first := waitForDetailDelivery(t, state)
	if !first.deadline.Equal(originalDeadline) || count.Load() != 1 {
		t.Fatalf("lost command deadline/collection: %+v count=%d", first, count.Load())
	}

	if state.take(now) != nil {
		t.Fatal("same request sent concurrently")
	}

	state.finish(first.id)

	retry := state.take(now.Add(2 * time.Second))
	if retry == nil || !bytes.Equal(first.payload, retry.payload) || count.Load() != 1 {
		t.Fatal("retry recollected or changed the snapshot")
	}

	state.acknowledge(&statusv1alpha1.NodeStatusAck{Status: "ok", DetailRequestID: first.id})
	state.finish(first.id)

	if state.replies[first.id].payload != nil || !state.replies[first.id].done {
		t.Fatal("ACK retained heavy payload")
	}

	if err := state.enqueue(req, now); err != nil {
		t.Fatal(err)
	}

	if state.take(now.Add(3*time.Second)) != nil || count.Load() != 1 {
		t.Fatal("delayed duplicate recollected an acknowledged request")
	}

	state.take(originalDeadline)

	if len(state.replies) != 0 {
		t.Fatal("deadline did not remove idempotency marker")
	}
}

func TestDetailStateExpiryAndConcurrentClaims(t *testing.T) {
	state := (&nodeHealthState{}).detailState()

	now := time.Now()
	if state.enqueue(&statusv1alpha1.DetailRequest{RequestID: "old", Deadline: now}, now) == nil {
		t.Fatal("expired command accepted")
	}

	req := &statusv1alpha1.DetailRequest{RequestID: "one", Deadline: now.Add(time.Minute)}

	var (
		collections atomic.Int32
		wg          sync.WaitGroup
	)

	startTestDetailWorker(t, state, "node", func() *NodeStatusResponse {
		collections.Add(1)

		return &NodeStatusResponse{}
	})

	for range 8 {
		wg.Go(func() {
			if err := state.enqueue(req, now); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	waitForStatusCondition(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()

		reply := state.replies[req.RequestID]

		return reply != nil && reply.ready
	})

	var deliveries atomic.Int32

	for range 8 {
		wg.Go(func() {
			if state.take(time.Now()) != nil {
				deliveries.Add(1)
			}
		})
	}

	wg.Wait()

	if collections.Load() != 1 || deliveries.Load() != 1 {
		t.Fatalf("collections=%d deliveries=%d, want 1 each", collections.Load(), deliveries.Load())
	}

	state.take(req.Deadline)

	if len(state.replies) != 0 {
		t.Fatal("expired unacknowledged detail retained")
	}
}

func TestDetailCollectionDoesNotBlockPublisherOrShutdown(t *testing.T) {
	state := (&nodeHealthState{}).detailState()
	started := make(chan struct{})
	release := make(chan struct{})
	done := startTestDetailWorker(t, state, "node", func() *NodeStatusResponse {
		close(started)
		<-release

		return &NodeStatusResponse{}
	})

	req := &statusv1alpha1.DetailRequest{RequestID: "slow", Deadline: time.Now().Add(30 * time.Millisecond)}
	if err := state.enqueue(req, time.Now()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("detail collection did not start")
	}

	takeStarted := time.Now()
	if delivery := state.take(time.Now()); delivery != nil {
		t.Fatal("unfinished collection was delivered")
	}

	if elapsed := time.Since(takeStarted); elapsed > 50*time.Millisecond {
		t.Fatalf("publisher blocked on detail collection for %v", elapsed)
	}

	time.Sleep(time.Until(req.Deadline) + 10*time.Millisecond)
	state.take(time.Now())

	state.mu.Lock()
	remaining := len(state.replies)
	state.mu.Unlock()

	if remaining != 0 {
		t.Fatal("expired collection retained its request")
	}

	stopStarted := time.Now()

	state.stop()

	if elapsed := time.Since(stopStarted); elapsed > 50*time.Millisecond {
		t.Fatalf("shutdown blocked on detail collection for %v", elapsed)
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detail worker did not stop after collection returned")
	}
}

func TestDetailDisconnectExpiresUnacknowledgedReply(t *testing.T) {
	state := (&nodeHealthState{}).detailState()
	startTestDetailWorker(t, state, "node", func() *NodeStatusResponse { return &NodeStatusResponse{} })

	req := &statusv1alpha1.DetailRequest{RequestID: "disconnected", Deadline: time.Now().Add(20 * time.Millisecond)}
	if err := state.enqueue(req, time.Now()); err != nil {
		t.Fatal(err)
	}

	if waitForDetailDelivery(t, state) == nil {
		t.Fatal("missing unacknowledged reply")
	}

	waitForStatusCondition(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()

		return len(state.replies) == 0
	})
}

func TestDetailPayloadErrorsAreCorrelated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		collect func() *NodeStatusResponse
	}{
		{"nil", func() *NodeStatusResponse { return nil }},
		{"panic", func() *NodeStatusResponse { panic("failed syscall") }},
		{"fetch error", func() *NodeStatusResponse { return &NodeStatusResponse{FetchError: "unavailable"} }},
		{"oversized", func() *NodeStatusResponse {
			return &NodeStatusResponse{NodeErrors: []NodeError{{Message: strings.Repeat("x", nodeDetailFrameLimit)}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := collectDetailPayload("node", "request", tc.collect)
			payload = (&nodeHealthState{}).detailState().wsPayload("node", &nodeDetailDelivery{id: "request", payload: payload})

			var message statusproto.NodeStatusMessage
			if err := proto.Unmarshal(payload, &message); err != nil {
				t.Fatal(err)
			}

			if message.Type != statusv1alpha1.NodeStatusDetailsType || message.NodeName != "node" ||
				message.DetailRequestId != "request" || message.DetailError == "" || message.Status != nil || len(payload) >= nodeDetailFrameLimit {
				t.Fatalf("invalid correlated failure: %v", &message)
			}
		})
	}
}
