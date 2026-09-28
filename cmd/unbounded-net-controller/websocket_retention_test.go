// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

func TestWebSocketBroadcastHistoryIsOverviewOnly(t *testing.T) {
	health := &healthState{}
	health.clusterStatusCache = NewClusterStatusCache(health)
	health.clusterStatusCache.status = &ClusterStatusResponse{}
	full := retentionFixture(10000)
	health.clusterStatusCache.PatchNode("node", full)
	broadcaster := NewWSBroadcaster(health)

	clients := []*WSClient{
		{send: make(chan []byte, 4)},
		{send: make(chan []byte, 4), summarySubscribed: true},
	}
	for _, client := range clients {
		broadcaster.Register(client)
	}

	for _, messageType := range []string{"cluster_summary", "cluster_summary_delta"} {
		if messageType == "cluster_summary_delta" {
			full.NodeInfo.SiteName = "updated"
			health.clusterStatusCache.PatchNode("node", full)
		}

		broadcaster.broadcastUpdate(t.Context())

		for _, client := range clients {
			if len(client.send) != 1 {
				t.Fatal("global update emitted extra detail messages")
			}

			payload := <-client.send

			var envelope struct {
				Type string                     `json:"type"`
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(payload, &envelope); err != nil {
				t.Fatal(err)
			}

			if envelope.Type != messageType || envelope.Data["nodes"] != nil || envelope.Data["nodeSummaries"] == nil ||
				strings.Contains(string(payload), "private-detail-marker") || strings.Contains(string(payload), "routeDistances") {
				t.Fatal("global frame was not summary-only")
			}
		}

		history, err := json.Marshal(broadcaster.lastSummary)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(history), "private-detail-marker") || len(broadcaster.lastSummary.NodeSummaries) != 1 {
			t.Fatal("broadcast history retained node details")
		}
	}
}

func TestWebSocketBroadcastConcurrentUnregister(t *testing.T) {
	health := &healthState{}
	health.clusterStatusCache = NewClusterStatusCache(health)
	health.clusterStatusCache.status = &ClusterStatusResponse{}
	broadcaster := NewWSBroadcaster(health)

	var workers sync.WaitGroup

	workers.Go(func() {
		for range 100 {
			client := &WSClient{send: make(chan []byte, 1)}
			broadcaster.Register(client)
			broadcaster.Unregister(client)
		}
	})
	workers.Go(func() {
		for range 100 {
			health.clusterStatusCache.PatchNode("node", NodeStatusResponse{NodeInfo: NodeInfo{Name: "node"}})
			broadcaster.broadcastUpdate(t.Context())
		}
	})
	workers.Wait()
}

func TestWebSocketBroadcastShutdownReleasesHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broadcaster := NewWSBroadcaster(&healthState{})
		broadcaster.lastSummary = &ClusterSummary{NodeSummaries: []NodeSummary{{Name: "node"}}}
		clientCtx, clientCancel := context.WithCancel(t.Context())
		broadcaster.Register(&WSClient{ctx: clientCtx, cancel: clientCancel, send: make(chan []byte, 1)})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			defer close(done)

			broadcaster.Run(ctx)
		}()

		synctest.Wait()
		cancel()
		<-done

		if broadcaster.lastSummary != nil || clientCtx.Err() == nil {
			t.Fatal("shutdown retained history or left client active")
		}
	})
}

func TestWebSocketClosedClientCannotReceiveOrRegister(t *testing.T) {
	broadcaster := NewWSBroadcaster(nil)
	client := &WSClient{send: make(chan []byte, 1)}
	broadcaster.Register(client)
	broadcaster.Unregister(client)
	broadcaster.sendToClient(client, WSMessage{Type: "node_detail_response"})
	broadcaster.Register(client)

	if broadcaster.ClientCount() != 0 || len(client.send) != 0 {
		t.Fatal("closed client was revived")
	}
}
