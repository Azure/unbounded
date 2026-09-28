// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/apimachinery/pkg/types"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func testDetailRequests(t *testing.T, hooks nodeDetailRequestHooks) *nodeDetailRequests {
	t.Helper()

	if hooks.Resolve == nil {
		hooks.Resolve = func(string) (types.UID, error) { return "uid", nil }
	}

	cache, err := newNodeDetailCache(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	manager, err := newNodeDetailRequests(t.Context(), cache, 3*time.Second, hooks)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(manager.Close)

	return manager
}

func testDetailStatus() *NodeStatusResponse {
	return &NodeStatusResponse{Timestamp: time.Now(), NodeInfo: NodeInfo{Name: "node"}}
}

func TestDetailRequestsValidation(t *testing.T) {
	cache, _ := newNodeDetailCache(time.Second)
	resolve := func(string) (types.UID, error) { return "uid", nil }

	for _, tc := range []struct {
		cache   *nodeDetailCache
		timeout time.Duration
		hooks   nodeDetailRequestHooks
	}{
		{nil, time.Second, nodeDetailRequestHooks{Resolve: resolve}},
		{cache, 0, nodeDetailRequestHooks{Resolve: resolve}},
		{cache, -time.Second, nodeDetailRequestHooks{Resolve: resolve}},
		{cache, time.Second, nodeDetailRequestHooks{}},
	} {
		if _, err := newNodeDetailRequests(t.Context(), tc.cache, tc.timeout, tc.hooks); err == nil {
			t.Fatal("invalid constructor accepted")
		}
	}
}

func TestDetailRequestsCoalesceAndDispatch(t *testing.T) {
	for _, activeWS := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "WebSocket"}[activeWS], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var pulls, sends atomic.Int32

				manager := testDetailRequests(t, nodeDetailRequestHooks{
					Dispatch: func(context.Context, string, statusv1alpha1.DetailRequest) (bool, error) {
						sends.Add(1)

						return activeWS, nil
					},
					Pull: func(ctx context.Context, _ string) (*NodeStatusResponse, error) {
						pulls.Add(1)
						<-ctx.Done()

						return nil, ctx.Err()
					},
				})
				first := manager.Request("node", false)

				var workers sync.WaitGroup

				for range 32 {
					workers.Go(func() {
						result := manager.Request("node", true)
						if result.State != statusv1alpha1.NodeDetailPending || result.RequestID != first.RequestID || result.Deadline != first.Deadline {
							t.Error("concurrent refresh did not coalesce")
						}
					})
				}

				workers.Wait()
				synctest.Wait()

				if sends.Load() != 1 || pulls.Load() != map[bool]int32{false: 1, true: 0}[activeWS] {
					t.Fatal("unexpected dispatch count")
				}

				if _, ok := manager.Pending("node"); ok {
					t.Fatal("poll command available before pull failure")
				}
			})
		})
	}
}

func TestDetailRequestsCacheRefreshAndDuplicate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		request := manager.Request("node", false)

		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		original := manager.Result("node", request.RequestID)

		time.Sleep(time.Second)

		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		duplicate := manager.Request("node", false)
		if duplicate.State != statusv1alpha1.NodeDetailComplete || duplicate.Details == nil ||
			*duplicate.Details != *original.Details {
			t.Fatal("duplicate changed data or receipt TTL")
		}

		refresh := manager.Request("node", true)
		if refresh.RequestID == request.RequestID || refresh.State != statusv1alpha1.NodeDetailPending {
			t.Fatal("refresh did not create a fresh request")
		}

		if cached := manager.Request("node", false); cached.RequestID != request.RequestID || cached.Details == nil {
			t.Fatal("pending refresh prevented reuse of valid data")
		}

		if err := manager.Complete("node", refresh.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		if old := manager.Result("node", request.RequestID); old.State != statusv1alpha1.NodeDetailExpired || old.Details != nil {
			t.Fatal("old request retained a second result")
		}

		time.Sleep(10 * time.Second)
		synctest.Wait()

		if result := manager.Result("node", refresh.RequestID); result.State != statusv1alpha1.NodeDetailExpired || result.Details != nil {
			t.Fatal("result still available at TTL boundary")
		}

		assertNodeDetailEntries(t, manager.cache, 0)
	})
}

func TestDetailRequestsFallbackDeadlineAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Dispatch: func(context.Context, string, statusv1alpha1.DetailRequest) (bool, error) {
				return true, errors.New("socket closed")
			},
			Pull: func(ctx context.Context, _ string) (*NodeStatusResponse, error) {
				deadline, ok := ctx.Deadline()
				if !ok || deadline != time.Now().Add(3*time.Second) {
					t.Error("pull did not inherit the overall deadline")
				}

				time.Sleep(time.Second)

				return nil, errors.New("unreachable")
			},
		})
		request := manager.Request("node", false)

		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()

		for range 2 {
			command, ok := manager.Pending("node")
			if !ok || command.RequestID != request.RequestID || command.Deadline != request.Deadline {
				t.Fatal("failed pull did not expose the unchanged polling command")
			}
		}

		time.Sleep(2*time.Second - time.Nanosecond)

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailPending {
			t.Fatal("request expired too early")
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailExpired {
			t.Fatal("request remained pending at deadline")
		}

		if _, ok := manager.Pending("node"); ok {
			t.Fatal("expired polling command retained")
		}

		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err == nil {
			t.Fatal("late result accepted")
		}

		assertNodeDetailEntries(t, manager.cache, 0)
		time.Sleep(manager.timeout)
		synctest.Wait()
		manager.mu.Lock()
		count := len(manager.requests) + len(manager.active)
		manager.mu.Unlock()

		if count != 0 {
			t.Fatal("terminal metadata was not proactively removed")
		}
	})
}

func TestDetailRequestsBindingAndDeletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		uid := types.UID("old")
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Resolve: func(string) (types.UID, error) { return uid, nil },
		})
		request := manager.Request("node", false)

		for _, status := range []*NodeStatusResponse{nil, {}, {NodeInfo: NodeInfo{Name: "wrong"}}, {NodeInfo: NodeInfo{Name: "node"}, FetchError: "failed"}} {
			if err := manager.Complete("node", request.RequestID, status); err == nil {
				t.Fatal("invalid details accepted")
			}
		}

		if err := manager.Complete("other", request.RequestID, testDetailStatus()); err == nil {
			t.Fatal("wrong node binding accepted")
		}

		if err := manager.Complete("node", "unknown", testDetailStatus()); err == nil {
			t.Fatal("unknown request accepted")
		}

		synctest.Wait()

		uid = "replacement"

		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err == nil {
			t.Fatal("replaced node accepted")
		}

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailUnavailable {
			t.Fatal("replacement did not invalidate request")
		}

		fresh := manager.Request("node", false)
		manager.InvalidateNode("node", "old")

		if err := manager.Complete("node", fresh.RequestID, testDetailStatus()); err != nil {
			t.Fatalf("old informer event invalidated new node: %v", err)
		}

		manager.InvalidateNode("node", "replacement")
		assertNodeDetailEntries(t, manager.cache, 0)

		if result := manager.Result("node", fresh.RequestID); result.State != statusv1alpha1.NodeDetailUnavailable {
			t.Fatal("node deletion did not invalidate cached details")
		}
	})
}

func TestDetailRequestsHTTPCompletionAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Pull: func(context.Context, string) (*NodeStatusResponse, error) { return testDetailStatus(), nil },
		})
		request := manager.Request("node", false)

		synctest.Wait()

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailComplete || result.Details == nil {
			t.Fatal("HTTP pull did not complete")
		}

		manager.Close()

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailRetryable || result.Details != nil {
			t.Fatal("shutdown did not make results retryable")
		}

		if result := manager.Request("node", true); result.State != statusv1alpha1.NodeDetailRetryable {
			t.Fatal("shutdown accepted a new request")
		}

		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err == nil {
			t.Fatal("shutdown accepted late details")
		}

		assertNodeDetailEntries(t, manager.cache, 0)
		restarted := testDetailRequests(t, nodeDetailRequestHooks{})

		if result := restarted.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailRetryable {
			t.Fatal("new leader pretended to own an old request")
		}

		if result := restarted.Request("node", false); result.RequestID == request.RequestID {
			t.Fatal("restart reused an old request ID")
		}
	})
}
