// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"testing"
	"time"
)

// TestNodeStatusCacheApplyDelta tests NodeStatusCacheApplyDelta.
func TestNodeStatusCacheApplyDelta(t *testing.T) {
	t.Run("missing entry requires full refresh", func(t *testing.T) {
		cache := NewNodeStatusCache()

		rev, needFull, err := cache.ApplyDelta("node-missing", 1, map[string]json.RawMessage{"healthCheck": json.RawMessage(`{"healthy":true}`)}, "push")
		if err != nil {
			t.Fatalf("ApplyDelta returned error: %v", err)
		}

		if rev != 0 || !needFull {
			t.Fatalf("expected missing entry to request full refresh, got rev=%d needFull=%v", rev, needFull)
		}
	})

	t.Run("revision mismatch requires full refresh", func(t *testing.T) {
		cache := NewNodeStatusCache()
		cache.entries["node-a"] = &CachedNodeStatus{
			Status:   &NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}},
			Revision: 5,
		}

		rev, needFull, err := cache.ApplyDelta("node-a", 4, map[string]json.RawMessage{}, "push")
		if err != nil {
			t.Fatalf("ApplyDelta returned error: %v", err)
		}

		if rev != 5 || !needFull {
			t.Fatalf("expected revision mismatch to request full refresh with current rev, got rev=%d needFull=%v", rev, needFull)
		}
	})

	t.Run("invalid delta field returns error", func(t *testing.T) {
		cache := NewNodeStatusCache()
		cache.entries["node-a"] = &CachedNodeStatus{
			Status:   &NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}},
			Revision: 1,
		}

		_, needFull, err := cache.ApplyDelta("node-a", 1, map[string]json.RawMessage{"nodeInfo": json.RawMessage(`{bad json}`)}, "push")
		if err == nil {
			t.Fatalf("expected error for invalid delta JSON")
		}

		if needFull {
			t.Fatalf("expected decode error path to avoid needFull=true")
		}
	})

	t.Run("successful merge increments revision and applies typed fields", func(t *testing.T) {
		cache := NewNodeStatusCache()
		cache.entries["node-a"] = &CachedNodeStatus{
			Status: &NodeStatusResponse{
				NodeInfo:   NodeInfo{Name: "node-a", SiteName: "site-a"},
				FetchError: "old",
			},
			Revision: 1,
		}

		changed := make(chan struct{}, 1)

		cache.SetOnChange(func(_ string, _ *NodeStatusResponse) { changed <- struct{}{} })

		delta := map[string]json.RawMessage{
			"healthCheck": json.RawMessage(`{"healthy":true,"summary":"ok"}`),
			"fetchError":  json.RawMessage(`null`),
			"nodeInfo":    json.RawMessage(`{"name":"node-a","siteName":"site-b"}`),
		}

		rev, needFull, err := cache.ApplyDelta("node-a", 1, delta, "delta")
		if err != nil {
			t.Fatalf("ApplyDelta returned error: %v", err)
		}

		if needFull {
			t.Fatalf("expected successful merge to avoid full refresh")
		}

		if rev != 2 {
			t.Fatalf("expected revision to increment to 2, got %d", rev)
		}

		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatalf("expected onChange callback after successful ApplyDelta")
		}

		entry, ok := cache.Get("node-a")
		if !ok {
			t.Fatalf("expected merged cache entry")
		}

		if entry.Source != "delta" {
			t.Fatalf("expected source=delta, got %q", entry.Source)
		}

		if entry.Status.NodeInfo.SiteName != "site-b" {
			t.Fatalf("expected nodeInfo.siteName=site-b, got %q", entry.Status.NodeInfo.SiteName)
		}

		if entry.Status.NodeInfo.Name != "node-a" {
			t.Fatalf("expected node name preserved, got %q", entry.Status.NodeInfo.Name)
		}

		if entry.Status.FetchError != "" {
			t.Fatalf("expected null-deleted fetchError to be cleared, got %q", entry.Status.FetchError)
		}

		if entry.Status.HealthCheck == nil || !entry.Status.HealthCheck.Healthy {
			t.Fatalf("expected healthCheck to be applied, got %+v", entry.Status.HealthCheck)
		}
	})

	t.Run("unknown delta fields are silently ignored", func(t *testing.T) {
		cache := NewNodeStatusCache()
		cache.entries["node-a"] = &CachedNodeStatus{
			Status:   &NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}},
			Revision: 1,
		}

		delta := map[string]json.RawMessage{
			"futureField": json.RawMessage(`"value"`),
		}

		rev, needFull, err := cache.ApplyDelta("node-a", 1, delta, "push")
		if err != nil {
			t.Fatalf("ApplyDelta returned error for unknown field: %v", err)
		}

		if needFull {
			t.Fatalf("expected unknown field to not require full refresh")
		}

		if rev != 2 {
			t.Fatalf("expected revision to increment to 2, got %d", rev)
		}
	})
}
