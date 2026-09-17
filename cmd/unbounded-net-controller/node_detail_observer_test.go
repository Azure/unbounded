// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestLegacyObserverBridgeAuthenticatedPublishAndCachedDetails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var pulls atomic.Int32

		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Pull: func(context.Context, string) (*NodeStatusResponse, error) {
				pulls.Add(1)

				return nil, errors.New("node HTTP endpoint is unreachable")
			},
		})
		health := &healthState{
			statusCache: NewNodeStatusCache(), detailRequests: manager,
			nodeTokenVerifier: fakeServiceAccountTokenVerifier{},
		}
		health.isLeader.Store(true)
		health.statusCache.ObserveLegacyDetails(manager)

		fullCallbacks := 0

		health.statusCache.SetOnChange(func(_ string, status *NodeStatusResponse, _ uint64) {
			fullCallbacks++

			if len(status.Peers) == 0 {
				t.Error("bridge replaced the existing full callback payload")
			}
		})

		issuer := testTokenIssuer(t)
		token := testNodeToken(t, issuer)
		mux := http.NewServeMux()
		registerPushHandlers(mux, health, nil, make(chan struct{}, maxConcurrentNodeWS), issuer)
		registerStatusHandlers(mux, health, false, nil, nil, nil)

		fullBody := `{"mode":"full","nodeName":"node-a","status":{"nodeInfo":{"name":"node-a"},"peers":[{"name":"peer-1"}]}}`

		publish := func(body, bearer string) int {
			request := httptest.NewRequest(http.MethodPost, "/status/push", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+bearer)

			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			return response.Code
		}
		if code := publish(fullBody, "invalid"); code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated publication returned %d", code)
		}

		assertNodeDetailEntries(t, manager.cache, 0)

		if code := publish(fullBody, token); code != http.StatusOK {
			t.Fatalf("full publication returned %d", code)
		}

		response, first := serveDetailRequest(t, mux, http.MethodPost, "/status/node/node-a/details", "{}")
		if response.Code != http.StatusOK || first.State != statusv1alpha1.NodeDetailComplete ||
			first.Details == nil || first.Details.Status.Peers[0].Name != "peer-1" || pulls.Load() != 0 {
			t.Fatal("cached authenticated details required a direct HTTP pull")
		}

		time.Sleep(time.Second)

		deltaBody := `{"mode":"delta","nodeName":"node-a","baseRevision":1,"delta":{"peers":[{"name":"peer-2"},{"name":"peer-3"}]}}`
		if code := publish(deltaBody, token); code != http.StatusOK {
			t.Fatalf("delta publication returned %d", code)
		}

		current := manager.Request("node-a", false)
		if current.RequestID != first.RequestID || current.Details == nil || len(current.Details.Status.Peers) != 2 ||
			!current.Details.ExpiresAt.Equal(first.Details.ExpiresAt.Add(time.Second)) || pulls.Load() != 0 {
			t.Fatal("delta did not refresh the existing cache association")
		}

		legacy, ok := health.statusCache.Get("node-a")
		if !ok || legacy.Overview != nil || len(legacy.Status.Peers) != 2 || fullCallbacks != 2 {
			t.Fatal("bridge changed existing full-cache or callback behavior")
		}

		health.statusCache.UpdateSource("node-a", "ws")

		if _, err := health.statusCache.StoreOverview("summary-node", statusv1alpha1.NodeStatusOverview{
			NodeInfo: NodeInfo{Name: "summary-node"},
		}, "push"); err != nil {
			t.Fatal(err)
		}

		after, _ := manager.cache.Get("node-a")
		if after.ExpiresAt != current.Details.ExpiresAt || fullCallbacks != 3 {
			t.Fatal("source/summary change renewed the detail TTL or changed callbacks")
		}

		fresh := manager.Request("node-a", true)

		if code := publish(fullBody, token); code != http.StatusOK {
			t.Fatalf("next full publication returned %d", code)
		}

		pending := manager.Result("node-a", fresh.RequestID)
		if pending.State != statusv1alpha1.NodeDetailPending || pending.RequestID != fresh.RequestID || pending.Deadline != fresh.Deadline {
			t.Fatal("legacy publication reset or completed a pending correlated refresh")
		}

		time.Sleep(manager.cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, manager.cache, 0)

		legacy, ok = health.statusCache.Get("node-a")

		if !ok || len(legacy.Status.Peers) != 1 || legacy.Overview != nil {
			t.Fatal("bridge prematurely removed the legacy full base")
		}
	})
}

func TestLegacyObserverBridgeFailurePreservesOldStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		cache := NewNodeStatusCache()
		cache.ObserveLegacyDetails(manager)

		status := NodeStatusResponse{Peers: []WireGuardPeerStatus{{Name: "peer"}}}
		cache.StoreFull("node", status, "ws")

		details := manager.Request("node", false)

		if details.Details == nil || details.Details.Status.NodeInfo.Name != "node" ||
			status.NodeInfo.Name != "" || cache.entries["node"].Status.NodeInfo.Name != "" {
			t.Fatal("bridge failed to normalize only its own shallow snapshot")
		}

		manager.Close()

		if revision := cache.StoreFull("node", status, "ws"); revision != 2 {
			t.Fatal("observer failure changed old cache revision behavior")
		}

		if len(cache.entries["node"].Status.Peers) != 1 {
			t.Fatal("observer failure discarded the old full payload")
		}

		cache.ObserveLegacyDetails(nil)

		if revision := cache.StoreFull("node", status, "ws"); revision != 3 {
			t.Fatal("detaching the bridge changed legacy storage")
		}
	})
}
