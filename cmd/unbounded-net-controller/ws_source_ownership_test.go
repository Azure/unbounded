// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
)

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

func TestWebSocketWriteContextStopsWithEitherOwner(t *testing.T) {
	for _, cancelConnection := range []bool{false, true} {
		name := "request"
		if cancelConnection {
			name = "connection"
		}

		t.Run(name, func(t *testing.T) {
			requestCtx, cancelRequest := context.WithCancel(t.Context())
			connectionCtx, stopConnection := context.WithCancel(t.Context())
			writeCtx, cancelWrite := withConnectionContext(requestCtx, connectionCtx)

			t.Cleanup(func() {
				cancelWrite()
				cancelRequest()
				stopConnection()
			})

			if cancelConnection {
				stopConnection()
			} else {
				cancelRequest()
			}

			select {
			case <-writeCtx.Done():
			case <-t.Context().Done():
				t.Fatal("write context was not canceled")
			}
		})
	}
}
