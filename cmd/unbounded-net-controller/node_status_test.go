// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNodeStatusCacheOperations tests NodeStatusCacheOperations.
func TestNodeStatusCacheOperations(t *testing.T) {
	cache := NewNodeStatusCache()
	changed := make(chan struct{}, 2)

	cache.SetOnChange(func(_ string, _ *NodeStatusResponse) {
		_ = cache.GetAll()

		changed <- struct{}{}
	})

	statusA := NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a", SiteName: "site-a"}}
	statusB := NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-b", SiteName: "site-a"}}

	cache.StoreFull("node-a", statusA, "push")
	cache.StoreFull("node-b", statusB, "push")

	for i := 0; i < 2; i++ {
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatalf("expected onChange callback after store")
		}
	}

	entry, ok := cache.Get("node-a")
	if !ok {
		t.Fatalf("expected node-a to be cached")
	}

	if entry.ReceivedAt.IsZero() {
		t.Fatalf("expected ReceivedAt to be set")
	}

	entry.Status.NodeInfo.SiteName = "modified"

	fresh, ok := cache.Get("node-a")
	if !ok || fresh.Status.NodeInfo.SiteName != "site-a" {
		t.Fatalf("expected Get to return a copy, got %#v", fresh)
	}

	// GetAll returns shared pointers -- verify map contains expected nodes.
	all := cache.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries in GetAll, got %d", len(all))
	}

	if all["node-a"] == nil || all["node-a"].Status.NodeInfo.Name != "node-a" {
		t.Fatalf("expected node-a in GetAll")
	}

	cache.Delete("node-b")

	if _, ok := cache.Get("node-b"); ok {
		t.Fatalf("expected node-b to be deleted")
	}

	cache.CleanupStaleEntries(map[string]bool{"node-a": true})

	if _, ok := cache.Get("node-a"); !ok {
		t.Fatalf("expected node-a to remain after stale cleanup")
	}

	cache.CleanupStaleEntries(map[string]bool{})

	if _, ok := cache.Get("node-a"); ok {
		t.Fatalf("expected node-a to be removed by stale cleanup")
	}
}

// TestNodeStatusCacheUpdateSource tests NodeStatusCacheUpdateSource.
func TestNodeStatusCacheUpdateSource(t *testing.T) {
	cache := NewNodeStatusCache()
	status := NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a", SiteName: "site-a"}}
	cache.StoreFull("node-a", status, "ws")

	entry, ok := cache.Get("node-a")
	if !ok {
		t.Fatalf("expected node-a to be cached")
	}

	receivedAt := entry.ReceivedAt

	if !cache.UpdateSource("node-a", "stale-cache") {
		t.Fatalf("expected UpdateSource to succeed for existing node")
	}

	updated, ok := cache.Get("node-a")
	if !ok {
		t.Fatalf("expected node-a to remain cached")
	}

	if updated.Source != "stale-cache" {
		t.Fatalf("expected source stale-cache, got %q", updated.Source)
	}

	if !updated.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("expected UpdateSource to preserve ReceivedAt timestamp")
	}

	if cache.UpdateSource("missing", "stale-cache") {
		t.Fatalf("expected UpdateSource to fail for missing node")
	}
}

// TestFetchNodeStatus tests FetchNodeStatus.
func TestFetchNodeStatus(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}})
		}))
		defer ts.Close()

		host, port := mustHostPort(t, ts.URL)

		status, err := fetchNodeStatus(context.Background(), host, port)
		if err != nil {
			t.Fatalf("fetchNodeStatus error: %v", err)
		}

		if status.NodeInfo.Name != "node-a" {
			t.Fatalf("unexpected node name: %#v", status)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusServiceUnavailable)
		}))
		defer ts.Close()

		host, port := mustHostPort(t, ts.URL)

		_, err := fetchNodeStatus(context.Background(), host, port)
		if err == nil || !strings.Contains(err.Error(), "unexpected status code") {
			t.Fatalf("expected status code error, got %v", err)
		}
	})

	t.Run("bad json", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{bad json"))
		}))
		defer ts.Close()

		host, port := mustHostPort(t, ts.URL)

		_, err := fetchNodeStatus(context.Background(), host, port)
		if err == nil || !strings.Contains(err.Error(), "failed to decode response") {
			t.Fatalf("expected decode error, got %v", err)
		}
	})
}

func mustHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	return host, port
}
