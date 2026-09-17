// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestWebSocketTeardownPreservesNewerStatusSource(t *testing.T) {
	health := &healthState{statusCache: NewNodeStatusCache()}
	status := NodeStatusResponse{NodeInfo: NodeInfo{Name: "node"}}
	health.statusCache.StoreFull("node", status, "ws")
	old := health.registerNodeWS("node", func() {})
	current := health.registerNodeWS("node", func() {})

	health.markNodeWSStale("node", old, "ws")

	if cached, _ := health.statusCache.Get("node"); cached.Source != "ws" {
		t.Fatal("old connection teardown marked the replacement stale")
	}

	health.statusCache.StoreFull("node", status, "push")
	health.markNodeWSStale("node", current, "ws")

	if cached, _ := health.statusCache.Get("node"); cached.Source != "push" {
		t.Fatal("WebSocket teardown overwrote a newer HTTP publication")
	}

	health.statusCache.StoreFull("node", status, "ws")
	before := health.statusCache.GetAll()
	health.markNodeWSStale("node", current, "ws")

	if cached, _ := health.statusCache.Get("node"); cached.Source != "stale-cache" || before["node"].Source != "ws" {
		t.Fatal("current teardown lost its stale signal or mutated an old snapshot")
	}
}
