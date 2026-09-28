// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestDetailDispatchUsesOnlyCurrentCapableConnection(t *testing.T) {
	health := &healthState{}

	var canceled, sent atomic.Int32

	cancel := func() { canceled.Add(1) }
	old := health.registerNodeWS("node", cancel)
	current := health.registerNodeWS("node", cancel)
	health.unregisterNodeWS("node", old)

	if canceled.Load() != 1 {
		t.Fatal("replaced connection was not canceled")
	}

	command := statusv1alpha1.DetailRequest{RequestID: "request", Deadline: time.Now().Add(time.Minute)}
	if ok, err := health.dispatchNodeDetail(t.Context(), "node", command); ok || err != nil {
		t.Fatal("legacy connection was treated as command-capable")
	}

	health.setNodeWSDetailSender("node", old, func(context.Context, statusv1alpha1.DetailRequest) error {
		t.Error("an obsolete connection sent a command")
		return nil
	})
	health.setNodeWSDetailSender("node", current, func(_ context.Context, got statusv1alpha1.DetailRequest) error {
		if got != command {
			t.Error("command identity or deadline changed")
		}

		sent.Add(1)

		return nil
	})

	if ok, err := health.dispatchNodeDetail(t.Context(), "node", command); !ok || err != nil || sent.Load() != 1 {
		t.Fatalf("current connection did not receive command: %v %v", ok, err)
	}

	health.unregisterNodeWS("node", current)

	if ok, _ := health.dispatchNodeDetail(t.Context(), "node", command); ok {
		t.Fatal("closed connection remained usable")
	}
}

func TestDetailDispatchDisconnectAndReconnectPreserveDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := &healthState{}
		commands := make(chan statusv1alpha1.DetailRequest, 3)
		sender := func(_ context.Context, command statusv1alpha1.DetailRequest) error {
			select {
			case commands <- command:
				return nil
			default:
				return errors.New("unexpected extra dispatch")
			}
		}

		var pulls atomic.Int32

		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Dispatch: health.dispatchNodeDetail,
			Pull: func(context.Context, string) (*NodeStatusResponse, error) {
				pulls.Add(1)
				return nil, errors.New("unreachable")
			},
		})
		health.detailRequests = manager
		connection := health.registerNodeWS("node", func() {})
		health.setNodeWSDetailSender("node", connection, sender)

		request := manager.Request("node", true)

		synctest.Wait()

		if len(commands) != 1 {
			t.Fatal("expected one WebSocket command")
		}

		first := <-commands
		if pulls.Load() != 0 || first.RequestID != request.RequestID {
			t.Fatal("active WebSocket did not take priority")
		}

		time.Sleep(time.Second)
		health.unregisterNodeWS("node", connection)
		synctest.Wait()

		pending, ok := manager.Pending("node")
		if !ok || pulls.Load() != 1 || pending != first {
			t.Fatal("disconnect failed to pull and retain the original POST command")
		}

		reconnected := health.registerNodeWS("node", func() {})
		health.setNodeWSDetailSender("node", reconnected, sender)
		synctest.Wait()

		if len(commands) != 1 {
			t.Fatal("expected one command on reconnect")
		}

		if next := <-commands; next != first {
			t.Fatal("reconnect reset request identity or deadline")
		}

		health.setNodeWSDetailSender("node", reconnected, sender)
		synctest.Wait()

		if len(commands) != 0 {
			t.Fatal("a routine capability update dispatched another collection")
		}

		if err := manager.Complete("node", first.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		health.unregisterNodeWS("node", reconnected)
		synctest.Wait()

		if pulls.Load() != 1 {
			t.Fatal("a completed request restarted after disconnect")
		}
	})
}

func TestDetailDispatchRetryCoalescesAndCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active, peak atomic.Int32

		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Dispatch: func(ctx context.Context, _ string, _ statusv1alpha1.DetailRequest) (bool, error) {
				n := active.Add(1)
				if n > peak.Load() {
					peak.Store(n)
				}

				defer active.Add(-1)

				<-ctx.Done()

				return false, ctx.Err()
			},
		})
		manager.Request("node", true)
		synctest.Wait()

		for range 10 {
			manager.Retry("node")
		}

		synctest.Wait()
		manager.Close()

		if peak.Load() != 1 || active.Load() != 0 {
			t.Fatal("retry created concurrent dispatches or shutdown left one running")
		}
	})
}

func TestDetailDispatchWriteTimeoutFallsBackWithinOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := &healthState{}
		connection := health.registerNodeWS("node", func() {})
		health.setNodeWSDetailSender("node", connection, func(ctx context.Context, _ statusv1alpha1.DetailRequest) error {
			<-ctx.Done()
			return ctx.Err()
		})

		var pulls atomic.Int32

		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Dispatch: health.dispatchNodeDetail,
			Pull: func(context.Context, string) (*NodeStatusResponse, error) {
				pulls.Add(1)
				return testDetailStatus(), nil
			},
		})
		manager.timeout = 20 * time.Second
		health.detailRequests = manager
		request := manager.Request("node", true)

		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()

		result := manager.Result("node", request.RequestID)
		if pulls.Load() != 1 || result.State != statusv1alpha1.NodeDetailComplete || !result.Deadline.Equal(request.Deadline) {
			t.Fatalf("failed WebSocket write did not fall back within the original deadline: %+v", result)
		}
	})
}
