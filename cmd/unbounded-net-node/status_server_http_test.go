// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"
)

func reserveTestPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve test port: %v", err)
	}

	defer func() { _ = ln.Close() }()

	return ln.Addr().(*net.TCPAddr).Port
}

func waitHTTPReady(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for health server at %s", url)
}

// TestStartHealthServerEndpoints tests StartHealthServerEndpoints.
func TestStartHealthServerEndpoints(t *testing.T) {
	port := reserveTestPort(t)
	cniConfigured := true
	h := &nodeHealthState{
		cniConfigured:   &cniConfigured,
		informersSynced: []cache.InformerSynced{func() bool { return true }},
	}

	t.Setenv("NODE_NAME", "node-from-env")

	go startHealthServer(port, h)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHTTPReady(t, baseURL+"/healthz")

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("unexpected /healthz response: code=%d body=%q", resp.StatusCode, string(body))
	}

	resp, err = http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}

	body, _ = io.ReadAll(resp.Body)

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("unexpected /readyz response: code=%d body=%q", resp.StatusCode, string(body))
	}

	for _, path := range []string{"/status", "/status/json"} {
		resp, err = http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("%s request failed: %v", path, err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected %s status: %d", path, resp.StatusCode)
		}

		var got NodeStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			_ = resp.Body.Close()

			t.Fatalf("decode %s response failed: %v", path, err)
		}

		_ = resp.Body.Close()

		if got.NodeInfo.Name != "node-from-env" {
			t.Fatalf("unexpected %s node name: %q", path, got.NodeInfo.Name)
		}

		if got.NodeInfo.BuildInfo == nil {
			t.Fatalf("expected build info in %s response", path)
		}
	}
}

// TestStartHealthServerUnreadyWhenInformerUnsynced tests that /readyz returns
// 503 when informers are not synced, while /healthz always returns 200.
func TestStartHealthServerUnreadyWhenInformerUnsynced(t *testing.T) {
	port := reserveTestPort(t)
	cniConfigured := false
	h := &nodeHealthState{
		cniConfigured:   &cniConfigured,
		informersSynced: []cache.InformerSynced{func() bool { return false }},
	}

	go startHealthServer(port, h)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHTTPReady(t, baseURL+"/healthz")

	// /healthz should always return 200 (liveness)
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected /healthz 200, got code=%d body=%q", resp.StatusCode, string(body))
	}

	// /readyz should return 503 when informers unsynced (readiness)
	resp, err = http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}

	body, _ = io.ReadAll(resp.Body)

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "informer caches not synced") {
		t.Fatalf("expected /readyz 503 when unsynced, got code=%d body=%q", resp.StatusCode, string(body))
	}
}

// TestNodeHealthStateSetGetStatusServer tests NodeHealthStateSetGetStatusServer.
func TestNodeHealthStateSetGetStatusServer(t *testing.T) {
	h := &nodeHealthState{}
	if h.getStatusServer() != nil {
		t.Fatalf("expected nil status server before set")
	}

	srv := &nodeStatusServer{cfg: &config{NodeName: "node-a"}}
	h.setStatusServer(srv)

	if got := h.getStatusServer(); got != srv {
		t.Fatalf("unexpected status server pointer: got=%p want=%p", got, srv)
	}
}

// TestStatusFallbackUsesCurrentEnvNodeName tests StatusFallbackUsesCurrentEnvNodeName.
func TestStatusFallbackUsesCurrentEnvNodeName(t *testing.T) {
	old, had := os.LookupEnv("NODE_NAME")

	defer func() {
		if had {
			_ = os.Setenv("NODE_NAME", old)
		} else {
			_ = os.Unsetenv("NODE_NAME")
		}
	}()

	_ = os.Setenv("NODE_NAME", "node-fallback")

	port := reserveTestPort(t)
	cniConfigured := true

	h := &nodeHealthState{cniConfigured: &cniConfigured, informersSynced: []cache.InformerSynced{func() bool { return true }}}
	go startHealthServer(port, h)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHTTPReady(t, baseURL+"/status")

	resp, err := http.Get(baseURL + "/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	var got NodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode status failed: %v", err)
	}

	if got.NodeInfo.Name != "node-fallback" {
		t.Fatalf("unexpected fallback node name: %q", got.NodeInfo.Name)
	}
}
