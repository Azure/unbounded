// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func detailTestViewer(t *testing.T, broadcaster *WSBroadcaster) *WSClient {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	client := &WSClient{ctx: ctx, cancel: cancel, send: make(chan []byte, 16)}
	broadcaster.Register(client)
	t.Cleanup(func() { broadcaster.Unregister(client) })

	return client
}

func TestViewerDetailsQueueMetadataAndExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var pulls atomic.Int32

		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Pull: func(context.Context, string) (*NodeStatusResponse, error) {
				pulls.Add(1)

				status := retentionFixture(1024)

				return &status, nil
			},
		})
		health := &healthState{detailRequests: manager}
		health.isLeader.Store(true)
		broadcaster := NewWSBroadcaster(health)
		requester := detailTestViewer(t, broadcaster)
		unrelated := detailTestViewer(t, broadcaster)

		broadcaster.rejectAutomaticDetails(requester, "node")

		if pulls.Load() != 0 {
			t.Fatal("automatic subscription triggered collection")
		}

		<-requester.send
		broadcaster.requestNodeDetails(requester, "node", true)
		synctest.Wait()

		if pulls.Load() != 1 || len(unrelated.send) != 0 {
			t.Fatal("request fanout or unrelated viewer response")
		}

		var complete []byte

		for len(requester.send) > 0 {
			data := <-requester.send

			var envelope struct {
				Data statusv1alpha1.NodeDetailResult `json:"data"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}

			if strings.Contains(string(data), "private-detail-marker") ||
				(envelope.Data.Details != nil && envelope.Data.Details.Status != nil) {
				t.Fatal("viewer queue retained serialized details")
			}

			if envelope.Data.State == statusv1alpha1.NodeDetailComplete {
				complete = data
			}
		}

		if complete == nil {
			t.Fatal("viewer did not receive completion metadata")
		}

		payload, writeCtx, cleanup, err := requester.prepareWrite(complete)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		if !strings.Contains(string(payload), "private-detail-marker") {
			t.Fatal("writer did not resolve current cached details")
		}

		time.Sleep(manager.cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, manager.cache, 0)

		if writeCtx.Err() == nil {
			t.Fatal("detail write context survived snapshot expiry")
		}

		requester.detailMu.Lock()
		watches := len(requester.detailWatches)
		requester.detailMu.Unlock()

		if watches != 0 || len(unrelated.send) != 0 {
			t.Fatal("expired observer retained state or notified unrelated viewer")
		}

		payload, _, expiredCleanup, err := requester.prepareWrite(complete)
		expiredCleanup()

		if err != nil || strings.Contains(string(payload), "private-detail-marker") || !strings.Contains(string(payload), `"state":"expired"`) {
			t.Fatal("stale queued metadata resurrected expired data")
		}

		if len(requester.send) == 0 || !strings.Contains(string(<-requester.send), `"state":"expired"`) {
			t.Fatal("viewer did not receive expiry metadata")
		}
	})
}

func TestViewerDetailObserverReplacementAndDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Pull: func(ctx context.Context, _ string) (*NodeStatusResponse, error) {
				<-ctx.Done()

				return nil, ctx.Err()
			},
		})
		health := &healthState{detailRequests: manager}
		health.isLeader.Store(true)
		broadcaster := NewWSBroadcaster(health)
		client := detailTestViewer(t, broadcaster)
		broadcaster.requestNodeDetails(client, "node", true)
		broadcaster.requestNodeDetails(client, "node", true)
		synctest.Wait()

		client.detailMu.Lock()
		watches := len(client.detailWatches)
		client.detailMu.Unlock()

		if watches != 1 {
			t.Fatal("repeated explicit request accumulated observers")
		}

		broadcaster.Unregister(client)
		synctest.Wait()
		client.detailMu.Lock()
		watches = len(client.detailWatches)
		client.detailMu.Unlock()

		if watches != 0 {
			t.Fatal("disconnected viewer retained an observer")
		}

		manager.mu.Lock()
		active := len(manager.active)
		manager.mu.Unlock()

		if active != 1 {
			t.Fatal("one disconnected viewer canceled the shared request")
		}
	})
}

func TestViewerDetailWriteStopsOnLeadershipLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})

		request := manager.Request("node", false)
		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		health := &healthState{detailRequests: manager}
		health.isLeader.Store(true)
		broadcaster := NewWSBroadcaster(health)
		client := detailTestViewer(t, broadcaster)
		broadcaster.sendDetailMetadata(client, manager.Result("node", request.RequestID))

		_, writeCtx, cleanup, err := client.prepareWrite(<-client.send)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		health.setLeader(false)
		synctest.Wait()

		if writeCtx.Err() == nil {
			t.Fatal("leadership loss left a detail write active")
		}
	})
}

func TestViewerDetailOldExpiryCannotExpireRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Pull: func(context.Context, string) (*NodeStatusResponse, error) { return testDetailStatus(), nil },
		})
		health := &healthState{detailRequests: manager}
		health.isLeader.Store(true)
		broadcaster := NewWSBroadcaster(health)
		client := detailTestViewer(t, broadcaster)
		broadcaster.requestNodeDetails(client, "node", true)
		synctest.Wait()
		time.Sleep(manager.cache.ttl / 2)
		broadcaster.requestNodeDetails(client, "node", true)
		synctest.Wait()

		for len(client.send) > 0 {
			<-client.send
		}

		time.Sleep(manager.cache.ttl / 2)
		synctest.Wait()

		if len(client.send) != 0 {
			t.Fatal("old timer emitted an expiry after refresh")
		}

		time.Sleep(manager.cache.ttl / 2)
		synctest.Wait()

		if len(client.send) != 1 || !strings.Contains(string(<-client.send), `"state":"expired"`) {
			t.Fatal("refreshed display did not expire on its own deadline")
		}
	})
}
