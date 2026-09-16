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

	count := 0
	collect := func() *NodeStatusResponse { count++; return h.getStatusSnapshot() }

	first := state.take("node-a", collect, now)
	if first == nil || !first.deadline.Equal(originalDeadline) || count != 1 {
		t.Fatalf("lost command deadline/collection: %+v count=%d", first, count)
	}

	if state.take("node-a", collect, now) != nil {
		t.Fatal("same request sent concurrently")
	}

	state.finish(first.id)

	retry := state.take("node-a", collect, now.Add(2*time.Second))
	if retry == nil || !bytes.Equal(first.payload, retry.payload) || count != 1 {
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

	if state.take("node-a", collect, now.Add(3*time.Second)) != nil || count != 1 {
		t.Fatal("delayed duplicate recollected an acknowledged request")
	}

	state.take("node-a", collect, originalDeadline)

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
		count atomic.Int32
		wg    sync.WaitGroup
	)
	for range 8 {
		wg.Go(func() {
			if err := state.enqueue(req, now); err != nil {
				t.Error(err)
			}

			state.take("node", func() *NodeStatusResponse {
				count.Add(1)
				return &NodeStatusResponse{}
			}, now)
		})
	}

	wg.Wait()

	if count.Load() != 1 {
		t.Fatalf("collected %d duplicate snapshots", count.Load())
	}

	state.take("node", nil, req.Deadline)

	if len(state.replies) != 0 {
		t.Fatal("expired unacknowledged detail retained")
	}
}

func TestDetailDeadlineExpiresDuringCollectionAndDisconnect(t *testing.T) {
	state := (&nodeHealthState{}).detailState()

	req := &statusv1alpha1.DetailRequest{RequestID: "slow", Deadline: time.Now().Add(20 * time.Millisecond)}
	if err := state.enqueue(req, time.Now()); err != nil {
		t.Fatal(err)
	}

	delivery := state.take("node", func() *NodeStatusResponse {
		<-time.After(time.Until(req.Deadline) + 10*time.Millisecond)
		return &NodeStatusResponse{}
	}, time.Now())
	if delivery != nil {
		t.Fatal("expired collection was delivered")
	}

	req = &statusv1alpha1.DetailRequest{RequestID: "disconnected", Deadline: time.Now().Add(20 * time.Millisecond)}
	if err := state.enqueue(req, time.Now()); err != nil {
		t.Fatal(err)
	}

	if state.take("node", func() *NodeStatusResponse { return &NodeStatusResponse{} }, time.Now()) == nil {
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
