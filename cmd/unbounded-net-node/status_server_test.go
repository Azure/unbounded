// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vishvananda/netlink"
)

var testStatusTime = time.Date(2026, time.February, 20, 0, 0, 0, 0, time.UTC)

func testNodeStatus(fetchError string) *NodeStatusResponse {
	return &NodeStatusResponse{
		Timestamp: testStatusTime,
		NodeInfo: NodeInfo{
			Name:     "node-a",
			SiteName: "site-a",
			PodCIDRs: []string{"10.244.0.0/24"},
		},
		Peers: []WireGuardPeerStatus{
			{
				Name: "peer-a",
				Tunnel: PeerTunnelStatus{
					Interface:     "wg0",
					PublicKey:     "pub-a",
					RxBytes:       101,
					TxBytes:       202,
					LastHandshake: testStatusTime,
				},
			},
		},
		FetchError: fetchError,
	}
}

// TestComputeStatusDelta tests ComputeStatusDelta.
func TestComputeStatusDelta(t *testing.T) {
	if delta, err := computeStatusDelta(nil, testNodeStatus("")); err != nil || delta != nil {
		t.Fatalf("expected nil delta for nil prev, got delta=%v err=%v", delta, err)
	}

	prev := testNodeStatus("old")
	currNodeOnly := testNodeStatus("old")

	currNodeOnly.NodeInfo.SiteName = "site-b"
	if delta, err := computeStatusDelta(prev, currNodeOnly); err != nil || delta != nil {
		t.Fatalf("expected nil delta for nodeInfo-only changes, got delta=%v err=%v", delta, err)
	}

	// FetchError going from "old" to "" is omitted by omitempty, so the
	// function treats it as a removed key and intentionally does NOT emit
	// null (see comment in computeStatusDelta). Delta should be nil since
	// the only populated entry would be nodeInfo (len == 1 -> nil).
	currCleared := testNodeStatus("")
	if delta, err := computeStatusDelta(prev, currCleared); err != nil || delta != nil {
		t.Fatalf("expected nil delta when only omitempty field cleared, got delta=%v err=%v", delta, err)
	}

	// A real non-nodeInfo change (fetchError value changed to a different
	// non-empty string) should produce a non-nil delta.
	currChanged := testNodeStatus("new-error")

	delta, err := computeStatusDelta(prev, currChanged)
	if err != nil {
		t.Fatalf("computeStatusDelta returned error: %v", err)
	}

	if delta == nil {
		t.Fatalf("expected non-nil delta when non-nodeInfo fields changed")
	}

	if _, ok := delta["nodeInfo"]; !ok {
		t.Fatalf("expected nodeInfo to always be included in delta")
	}

	fetchRaw, ok := delta["fetchError"]
	if !ok {
		t.Fatalf("expected fetchError in delta")
	}

	if string(fetchRaw) != `"new-error"` {
		t.Fatalf("expected fetchError to be %q, got %q", `"new-error"`, string(fetchRaw))
	}
}

// TestResolveStatusWebSocketURLs tests ResolveStatusWebSocketURLs.
func TestResolveStatusWebSocketURLs(t *testing.T) {
	t.Run("explicit websocket URL wins", func(t *testing.T) {
		cfg := &config{StatusWSURL: "wss://custom/ws", StatusWSAPIServerMode: statusWSAPIServerModePreferred}

		urls := resolveStatusWebSocketURLs(cfg, true)
		if len(urls) != 1 || urls[0] != "wss://custom/ws" {
			t.Fatalf("expected explicit websocket URL only, got %v", urls)
		}
	})

	t.Run("service and apiserver fallback", func(t *testing.T) {
		origHost, hadHost := os.LookupEnv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
		origPort, hadPort := os.LookupEnv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")
		origAPIHost, hadAPIHost := os.LookupEnv("KUBERNETES_SERVICE_HOST")

		t.Cleanup(func() {
			if hadHost {
				_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", origHost)
			} else {
				_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
			}

			if hadPort {
				_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", origPort)
			} else {
				_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")
			}

			if hadAPIHost {
				_ = os.Setenv("KUBERNETES_SERVICE_HOST", origAPIHost)
			} else {
				_ = os.Unsetenv("KUBERNETES_SERVICE_HOST")
			}
		})

		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "controller.svc")
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", "8080")
		_ = os.Setenv("KUBERNETES_SERVICE_HOST", "api.public.example")

		cfg := &config{StatusWSAPIServerMode: statusWSAPIServerModeFallback}

		urls := resolveStatusWebSocketURLs(cfg, true)
		if len(urls) != 2 {
			t.Fatalf("expected direct and apiserver websocket URLs, got %v", urls)
		}

		if urls[0] != "wss://controller.svc:8080/status/nodews" {
			t.Fatalf("unexpected direct websocket URL: %q", urls[0])
		}

		if urls[1] != "wss://api.public.example/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews" {
			t.Fatalf("unexpected apiserver websocket URL: %q", urls[1])
		}
	})

	t.Run("preferred mode prioritizes apiserver websocket URL", func(t *testing.T) {
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "controller.svc")
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", "8080")
		_ = os.Setenv("KUBERNETES_SERVICE_HOST", "api.public.example")

		t.Cleanup(func() {
			_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
			_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")
			_ = os.Unsetenv("KUBERNETES_SERVICE_HOST")
		})

		cfg := &config{
			StatusWSAPIServerMode: statusWSAPIServerModePreferred,
			StatusWSAPIServerURL:  "wss://$(KUBERNETES_SERVICE_HOST)/apis/custom.group/v1/status/nodews",
		}

		urls := resolveStatusWebSocketURLs(cfg, true)
		if len(urls) != 2 {
			t.Fatalf("expected preferred mode to include apiserver and direct URLs, got %v", urls)
		}

		if urls[0] != "wss://api.public.example/apis/custom.group/v1/status/nodews" {
			t.Fatalf("expected apiserver URL first in preferred mode, got %q", urls[0])
		}

		if !strings.Contains(urls[1], "/status/nodews") {
			t.Fatalf("expected direct URL second in preferred mode, got %q", urls[1])
		}
	})

	t.Run("legacy aggregated group URL is rewritten", func(t *testing.T) {
		cfg := &config{
			StatusWSAPIServerMode: statusWSAPIServerModePreferred,
			StatusWSAPIServerURL:  "wss://kubernetes.default.svc/apis/net.unbounded-cloud.io/v1alpha1/status/nodews",
		}

		urls := resolveStatusWebSocketURLs(cfg, true)
		if len(urls) == 0 {
			t.Fatalf("expected at least one websocket URL")
		}

		if urls[0] != "wss://kubernetes.default.svc/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews" {
			t.Fatalf("expected rewritten aggregated websocket URL, got %q", urls[0])
		}
	})

	t.Run("api server fallback can be suppressed", func(t *testing.T) {
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "controller.svc")
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", "8080")
		_ = os.Setenv("KUBERNETES_SERVICE_HOST", "api.public.example")

		t.Cleanup(func() {
			_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
			_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")
			_ = os.Unsetenv("KUBERNETES_SERVICE_HOST")
		})

		cfg := &config{StatusWSAPIServerMode: statusWSAPIServerModeFallback}

		urls := resolveStatusWebSocketURLs(cfg, false)
		if len(urls) != 1 {
			t.Fatalf("expected only direct websocket URL when fallback is suppressed, got %v", urls)
		}

		if urls[0] != "wss://controller.svc:8080/status/nodews" {
			t.Fatalf("unexpected direct websocket URL when fallback is suppressed: %q", urls[0])
		}
	})
}

// TestIsStatusAPIServerFallbackAllowed tests IsStatusAPIServerFallbackAllowed.
func TestIsStatusAPIServerFallbackAllowed(t *testing.T) {
	now := time.Now()
	startedAt := now.Add(-30 * time.Second)
	startedLongAgo := now.Add(-5 * time.Minute)

	if got := isStatusAPIServerFallbackAllowed(startedAt, time.Time{}, now, 60*time.Second, true); got {
		t.Fatalf("expected fallback to be blocked before startup delay elapses")
	}

	if got := isStatusAPIServerFallbackAllowed(startedLongAgo, time.Time{}, now, 60*time.Second, true); got {
		t.Fatalf("expected fallback to stay blocked until a direct outage has lasted for the delay")
	}

	if got := isStatusAPIServerFallbackAllowed(startedAt, now.Add(-5*time.Second), now, 60*time.Second, true); got {
		t.Fatalf("expected fallback to be blocked while direct path has been down for less than delay")
	}

	if got := isStatusAPIServerFallbackAllowed(startedAt, now.Add(-61*time.Second), now, 60*time.Second, true); !got {
		t.Fatalf("expected fallback to be allowed after direct-down delay elapses")
	}

	if got := isStatusAPIServerFallbackAllowed(startedAt, time.Time{}, now, 60*time.Second, false); !got {
		t.Fatalf("expected fallback to be allowed immediately when no direct path exists")
	}
}

// TestResolveStatusPushAPIServerURL tests ResolveStatusPushAPIServerURL.
func TestResolveStatusPushAPIServerURL(t *testing.T) {
	t.Run("derives https push URL from wss websocket URL", func(t *testing.T) {
		_ = os.Setenv("KUBERNETES_SERVICE_HOST", "api.public.example")

		t.Cleanup(func() {
			_ = os.Unsetenv("KUBERNETES_SERVICE_HOST")
		})

		cfg := &config{StatusWSAPIServerURL: "wss://$(KUBERNETES_SERVICE_HOST)/apis/custom.group/v1/status/nodews"}
		got := resolveStatusPushAPIServerURL(cfg)

		want := "https://api.public.example/apis/custom.group/v1/status/push"
		if got != want {
			t.Fatalf("unexpected push URL: got %q want %q", got, want)
		}
	})

	t.Run("derives http push URL from ws websocket URL", func(t *testing.T) {
		cfg := &config{StatusWSAPIServerURL: "ws://controller.svc/status/nodews"}
		got := resolveStatusPushAPIServerURL(cfg)

		want := "http://controller.svc/status/push"
		if got != want {
			t.Fatalf("unexpected push URL: got %q want %q", got, want)
		}
	})

	t.Run("rewrites legacy aggregated group in push URL", func(t *testing.T) {
		cfg := &config{StatusWSAPIServerURL: "wss://kubernetes.default.svc/apis/net.unbounded-cloud.io/v1alpha1/status/nodews"}
		got := resolveStatusPushAPIServerURL(cfg)

		want := "https://kubernetes.default.svc/apis/status.net.unbounded-cloud.io/v1alpha1/status/push"
		if got != want {
			t.Fatalf("unexpected push URL: got %q want %q", got, want)
		}
	})
}

// TestResolveDirectStatusURLs tests ResolveDirectStatusURLs.
func TestResolveDirectStatusURLs(t *testing.T) {
	origHost, hadHost := os.LookupEnv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
	origPort, hadPort := os.LookupEnv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")

	t.Cleanup(func() {
		if hadHost {
			_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", origHost)
		} else {
			_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
		}

		if hadPort {
			_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", origPort)
		} else {
			_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")
		}
	})

	t.Run("explicit direct URLs win", func(t *testing.T) {
		cfg := &config{
			StatusPushURL: "http://custom-controller/status/push",
			StatusWSURL:   "wss://custom-controller/status/nodews",
		}
		if got := resolveDirectStatusPushURL(cfg); got != cfg.StatusPushURL {
			t.Fatalf("expected explicit direct push URL, got %q", got)
		}

		if got := resolveDirectStatusWebSocketURL(cfg); got != cfg.StatusWSURL {
			t.Fatalf("expected explicit direct websocket URL, got %q", got)
		}
	})

	t.Run("service env vars are used when explicit URLs are absent", func(t *testing.T) {
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "controller.svc")
		_ = os.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", "8080")

		cfg := &config{}
		if got := resolveDirectStatusPushURL(cfg); got != "https://controller.svc:8080/status/push" {
			t.Fatalf("unexpected direct push URL: %q", got)
		}

		if got := resolveDirectStatusWebSocketURL(cfg); got != "wss://controller.svc:8080/status/nodews" {
			t.Fatalf("unexpected direct websocket URL: %q", got)
		}
	})

	t.Run("missing service env vars disables direct fallback URLs", func(t *testing.T) {
		_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
		_ = os.Unsetenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")

		cfg := &config{}
		if got := resolveDirectStatusPushURL(cfg); got != "" {
			t.Fatalf("expected empty direct push URL, got %q", got)
		}

		if got := resolveDirectStatusWebSocketURL(cfg); got != "" {
			t.Fatalf("expected empty direct websocket URL, got %q", got)
		}
	})
}

// TestStatusDirectRecoveryProbeInterval tests StatusDirectRecoveryProbeInterval.
func TestStatusDirectRecoveryProbeInterval(t *testing.T) {
	if got := statusDirectRecoveryProbeInterval(10 * time.Second); got != 30*time.Second {
		t.Fatalf("expected probe interval to be 3x status push interval, got %s", got)
	}

	if got := statusDirectRecoveryProbeInterval(0); got != 30*time.Second {
		t.Fatalf("expected default probe interval of 30s when status push interval is unset, got %s", got)
	}
}

// TestNextExponentialBackoff tests NextExponentialBackoff.
func TestNextExponentialBackoff(t *testing.T) {
	if got := nextExponentialBackoff(0, 30*time.Second); got != 2*time.Second {
		t.Fatalf("expected zero current backoff to normalize to 1s then double to 2s, got %s", got)
	}

	if got := nextExponentialBackoff(4*time.Second, 30*time.Second); got != 8*time.Second {
		t.Fatalf("expected backoff doubling to 8s, got %s", got)
	}

	if got := nextExponentialBackoff(20*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("expected backoff cap at 30s, got %s", got)
	}
}

// TestWebsocketHostScopeKey tests WebsocketHostScopeKey.
func TestWebsocketHostScopeKey(t *testing.T) {
	key := websocketHostScopeKey([]string{
		"ws://controller.svc:80/status/nodews",
		"wss://kubernetes.default.svc/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews",
	})
	if key != "controller.svc:80,kubernetes.default.svc" {
		t.Fatalf("unexpected websocket host scope key: %q", key)
	}

	badKey := websocketHostScopeKey([]string{"not a url"})
	if badKey != "not a url" {
		t.Fatalf("expected fallback to raw endpoint for unparsable URL, got %q", badKey)
	}
}

// TestStripPeerStatsAndSnapshot tests StripPeerStatsAndSnapshot.
func TestStripPeerStatsAndSnapshot(t *testing.T) {
	status := testNodeStatus("")
	stripped := stripPeerStats(status)

	if stripped == status {
		t.Fatalf("expected copy, got same pointer")
	}

	if len(stripped.Peers) != 1 {
		t.Fatalf("expected one peer, got %d", len(stripped.Peers))
	}

	peer := stripped.Peers[0]
	if peer.Tunnel.RxBytes != 0 || peer.Tunnel.TxBytes != 0 || !peer.Tunnel.LastHandshake.IsZero() {
		t.Fatalf("expected stats to be zeroed, got %#v", peer.Tunnel)
	}

	snapshot := peerStatsSnapshot(status)
	if len(snapshot) != 1 {
		t.Fatalf("expected snapshot entry, got %d", len(snapshot))
	}

	for key, v := range snapshot {
		if key != "peer-a|pub-a|wg0" || v[0] != 101 || v[1] != 202 {
			t.Fatalf("unexpected snapshot key/value: %q => %#v", key, v)
		}
	}
}

// TestRoutingRouteKeyNormalizesInputs was removed -- routingRouteKey was part of the FRR-based
// routing code path that has been replaced by kernel-based route collection.

// TestComputeStatusDelta_JsonRoundTripFields tests ComputeStatusDelta_JsonRoundTripFields.
func TestComputeStatusDelta_JsonRoundTripFields(t *testing.T) {
	prev := testNodeStatus("x")
	curr := testNodeStatus("y")

	delta, err := computeStatusDelta(prev, curr)
	if err != nil {
		t.Fatalf("computeStatusDelta returned error: %v", err)
	}

	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("failed to marshal delta: %v", err)
	}

	if len(raw) == 0 {
		t.Fatalf("expected non-empty marshaled delta")
	}
}

// TestGetNodeStatusBasicAndHandleStatusJSON tests GetNodeStatusBasicAndHandleStatusJSON.
func TestGetNodeStatusBasicAndHandleStatusJSON(t *testing.T) {
	s := &nodeStatusServer{
		cfg:    &config{NodeName: "node-a", WireGuardPort: 51820},
		pubKey: "pub-self",
		state: &wireGuardState{
			nodePodCIDRs:    []string{"10.244.0.0/24"},
			nodeInternalIPs: []string{"10.0.0.10"},
			nodeExternalIPs: []string{"20.0.0.10"},
			siteName:        "site-a",
		},
	}

	status := s.getNodeStatus()
	if status.NodeInfo.Name != "node-a" || status.NodeInfo.SiteName != "site-a" {
		t.Fatalf("unexpected node info: %#v", status.NodeInfo)
	}

	if status.NodeInfo.WireGuard == nil || status.NodeInfo.WireGuard.Interface != "wg51820" || status.NodeInfo.WireGuard.PublicKey != "pub-self" {
		t.Fatalf("unexpected wireguard status: %#v", status.NodeInfo.WireGuard)
	}

	if status.RoutingTable.Routes == nil {
		t.Fatalf("expected non-nil routing table slices")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status/json", nil)
	s.handleStatusJSON(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected content-type: %q", ct)
	}

	var decoded NodeStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode status JSON response: %v", err)
	}

	if decoded.NodeInfo.Name != "node-a" {
		t.Fatalf("unexpected decoded node name: %q", decoded.NodeInfo.Name)
	}
}

// TestGetNodeStatusIncludesNodeErrors tests GetNodeStatusIncludesNodeErrors.
func TestGetNodeStatusIncludesNodeErrors(t *testing.T) {
	s := &nodeStatusServer{
		cfg:    &config{NodeName: "node-a", WireGuardPort: 51820},
		pubKey: "pub-self",
		state: &wireGuardState{
			nodePodCIDRs:    []string{"10.244.0.0/24"},
			nodeInternalIPs: []string{"10.0.0.10"},
			nodeExternalIPs: []string{"20.0.0.10"},
			siteName:        "site-a",
			nodeErrors:      []NodeError{{Type: "directPush", Message: "request failed: dial tcp timeout"}, {Type: "fallbackPush", Message: "unexpected HTTP status: 503"}},
		},
	}

	status := s.getNodeStatus()
	if len(status.NodeErrors) != 2 {
		t.Fatalf("expected 2 node errors, got %#v", status.NodeErrors)
	}

	if status.NodeErrors[0].Message != "request failed: dial tcp timeout" {
		t.Fatalf("unexpected node error[0]: %#v", status.NodeErrors[0])
	}

	// Ensure response uses a copy so callers cannot mutate shared state.
	status.NodeErrors[0].Message = "changed"
	if s.state.nodeErrors[0].Message != "request failed: dial tcp timeout" {
		t.Fatalf("expected node errors to be copied before returning")
	}
}

// TestTryDirectRecoveryProbeClearsNodeErrors tests TryDirectRecoveryProbeClearsNodeErrors.
func TestTryDirectRecoveryProbeClearsNodeErrors(t *testing.T) {
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("failed to accept websocket connection: %v", err)
		}

		_ = conn.Close(websocket.StatusNormalClosure, "ok")
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
	health := &nodeHealthState{}
	health.setStatusServer(&nodeStatusServer{state: &wireGuardState{nodeErrors: []NodeError{{Type: "directWebsocket", Message: "node node-a direct websocket probe failed: dial tcp timeout"}}}})

	ok := tryDirectRecoveryProbe(context.Background(), health, &http.Client{Timeout: 5 * time.Second}, func() string { return "" }, wsURL, "node-a")
	if !ok {
		t.Fatalf("expected direct recovery probe to succeed")
	}

	srv := health.getStatusServer()
	if srv == nil || srv.state == nil {
		t.Fatalf("expected status server state to be available")
	}

	srv.state.mu.Lock()
	defer srv.state.mu.Unlock()

	if len(srv.state.nodeErrors) != 0 {
		t.Fatalf("expected node errors to be cleared after successful probe, got %#v", srv.state.nodeErrors)
	}
}

// TestCollectRoutingTableFromKernelEmptyState tests that collectRoutingTableFromKernel
// returns empty routes when no wg interfaces exist.
func TestCollectRoutingTableFromKernelEmptyState(t *testing.T) {
	s := &nodeStatusServer{state: &wireGuardState{}}

	info := s.collectRoutingTableFromKernel()
	if len(info.Routes) != 0 {
		t.Fatalf("expected empty routes without wg interfaces: %#v", info)
	}
}

// TestCollectRoutingTableFromKernelCached tests that cached routing table data is
// returned when the cache is valid and not dirty.
func TestCollectRoutingTableFromKernelCached(t *testing.T) {
	expected := RoutingTableInfo{
		Routes: []RouteEntry{
			{Destination: "10.0.0.0/24", Family: "IPv4"},
			{Destination: "fd00::/64", Family: "IPv6"},
		},
	}
	s := &nodeStatusServer{
		state:                &wireGuardState{},
		routingTableCache:    expected,
		routingTableCachedAt: time.Now(),
	}
	s.routingTableDirty.Store(false)

	info := s.collectRoutingTableFromKernel()
	if len(info.Routes) != 2 {
		t.Fatalf("expected 2 cached routes, got %d: %#v", len(info.Routes), info.Routes)
	}

	if info.Routes[0].Destination != "10.0.0.0/24" || info.Routes[0].Family != "IPv4" {
		t.Fatalf("expected cached IPv4 route, got %#v", info.Routes[0])
	}

	if info.Routes[1].Destination != "fd00::/64" || info.Routes[1].Family != "IPv6" {
		t.Fatalf("expected cached IPv6 route, got %#v", info.Routes[1])
	}
}

// TestStartRouteChangeWatcherMarksCacheDirty tests StartRouteChangeWatcherMarksCacheDirty.
func TestStartRouteChangeWatcherMarksCacheDirty(t *testing.T) {
	origSubscribe := routeSubscribeWithOptions

	t.Cleanup(func() {
		routeSubscribeWithOptions = origSubscribe
	})

	sent := make(chan struct{}, 1)
	routeSubscribeWithOptions = func(ch chan<- netlink.RouteUpdate, done <-chan struct{}, _ netlink.RouteSubscribeOptions) error {
		go func() {
			ch <- netlink.RouteUpdate{}

			sent <- struct{}{}

			<-done
		}()

		return nil
	}

	s := &nodeStatusServer{}
	s.routingTableDirty.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.startRouteChangeWatcher(ctx)
	<-sent

	deadline := time.Now().Add(500 * time.Millisecond)
	for !s.routingTableDirty.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if !s.routingTableDirty.Load() {
		t.Fatalf("expected route watcher to mark routing table cache dirty")
	}
}
