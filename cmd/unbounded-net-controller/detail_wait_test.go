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
	"testing/synctest"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestDetailWaitMetadataAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		request := manager.Request("node", false)
		_, changed := manager.WatchMetadata("node", request.RequestID)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if result := manager.Wait(ctx, "node", request.RequestID); result.State != statusv1alpha1.NodeDetailRetryable {
			t.Fatal("canceled waiter did not stop")
		}

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailPending {
			t.Fatal("one waiter canceled the shared request")
		}

		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		select {
		case <-changed:
		default:
			t.Fatal("completion did not notify metadata watchers")
		}

		metadata, changed := manager.WatchMetadata("node", request.RequestID)
		if metadata.Details == nil || metadata.Details.Status != nil {
			t.Fatal("watcher retained a full payload")
		}

		if result := manager.Wait(t.Context(), "node", request.RequestID); result.Details == nil || result.Details.Status == nil {
			t.Fatal("completed waiter did not return current details")
		}

		time.Sleep(manager.cache.ttl)
		synctest.Wait()

		select {
		case <-changed:
		default:
			t.Fatal("expiry did not notify metadata watchers")
		}

		if result := manager.Wait(t.Context(), "node", request.RequestID); result.State != statusv1alpha1.NodeDetailExpired {
			t.Fatal("waiter served expired details")
		}
	})
}

func TestLegacyNodeRouteUsesCommonRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})

		var pulls atomic.Int32

		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Pull: func(ctx context.Context, _ string) (*NodeStatusResponse, error) {
				pulls.Add(1)

				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return testDetailStatus(), nil
				}
			},
		})
		health := &healthState{detailRequests: manager, statusCache: NewNodeStatusCache()}
		health.isLeader.Store(true)
		health.statusCache.StoreFull("node", retentionFixture(3), "ws")

		mux := http.NewServeMux()
		registerStatusHandlers(mux, health, false, nil, nil, nil)

		first := manager.Request("node", true)
		done := make(chan *httptest.ResponseRecorder, 1)

		go func() {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status/node/node?live=true", nil))

			done <- response
		}()

		synctest.Wait()

		if pulls.Load() != 1 {
			t.Fatal("legacy live route started a competing pull")
		}

		close(release)

		response := <-done

		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "private-detail-marker") {
			t.Fatal("legacy route bypassed the TTL lifecycle")
		}

		response, _ = serveDetailRequest(t, mux, http.MethodGet, "/status/node/node", "")
		if response.Code != http.StatusOK || pulls.Load() != 1 {
			t.Fatal("legacy route did not reuse cached details")
		}

		response, _ = serveDetailRequest(t, mux, http.MethodGet, "/status/node/node?live=true", "")
		current, _ := manager.cache.Get("node")

		if response.Code != http.StatusOK || pulls.Load() != 2 || current.RequestID == first.RequestID {
			t.Fatal("live request did not force a common-manager refresh")
		}

		response, _ = serveDetailRequest(t, mux, http.MethodPost, "/status/node/node", "")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatal("legacy route accepted unsupported method")
		}
	})
}
