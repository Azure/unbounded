// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/Azure/unbounded/internal/net/authn"
	"github.com/Azure/unbounded/internal/net/webhook"
)

// testTokenIssuer creates a TokenIssuer for use in tests.
func testTokenIssuer(t *testing.T) *authn.TokenIssuer {
	t.Helper()

	key, err := authn.GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}

	issuer, err := authn.NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	return issuer
}

// testNodeToken issues a node HMAC token for use in tests.
func testNodeToken(t *testing.T, issuer *authn.TokenIssuer) string {
	t.Helper()

	token, _, err := issuer.IssueNodeToken("system:serviceaccount:unbounded-net:unbounded-net-node", "test-node", time.Hour)
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	return token
}

// TestRegisterStatusHandlers tests RegisterStatusHandlers.
func TestRegisterStatusHandlers(t *testing.T) {
	newHealth := func() *healthState {
		h := &healthState{
			clientset:      k8sfake.NewClientset(),
			statusCache:    NewNodeStatusCache(),
			staleThreshold: time.Minute,
			tokenAuth:      &tokenAuthenticator{tokenReviewer: k8sfake.NewClientset()},
		}
		h.isLeader.Store(true)
		h.pullEnabled.Store(false)
		// Pre-build the cluster status cache so /status/json returns immediately.
		cache := NewClusterStatusCache(h)
		cache.Rebuild(context.Background())
		h.clusterStatusCache = cache

		return h
	}

	t.Run("status json requires leader", func(t *testing.T) {
		h := newHealth()
		h.isLeader.Store(false)

		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/json", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 when not leader, got %d", resp.Code)
		}
	})

	t.Run("status json success", func(t *testing.T) {
		h := newHealth()
		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/json", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200 for /status/json, got %d body=%q", resp.Code, resp.Body.String())
		}

		if !strings.Contains(resp.Body.String(), "informers not ready") {
			t.Fatalf("expected response to include informer readiness error, got %q", resp.Body.String())
		}
	})

	t.Run("status node requires node name", func(t *testing.T) {
		h := newHealth()
		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing node name, got %d", resp.Code)
		}
	})

	t.Run("status node returns fresh cached payload", func(t *testing.T) {
		h := newHealth()

		rev := h.statusCache.StoreFull("node-a", NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}}, "push")
		if rev == 0 {
			t.Fatalf("expected cached revision")
		}

		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/node-a", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200 for fresh cache hit, got %d body=%q", resp.Code, resp.Body.String())
		}

		if !strings.Contains(resp.Body.String(), "node-a") {
			t.Fatalf("expected cached node payload, got %q", resp.Body.String())
		}
	})

	t.Run("status node stale cache while pull disabled", func(t *testing.T) {
		h := newHealth()
		h.staleThreshold = time.Second
		h.statusCache.entries["node-a"] = &CachedNodeStatus{
			Status:     &NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}},
			ReceivedAt: time.Now().Add(-2 * time.Minute),
			Source:     "push",
			Revision:   1,
		}
		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/node-a", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200 for stale cache response, got %d body=%q", resp.Code, resp.Body.String())
		}

		if !strings.Contains(resp.Body.String(), "stale status") || !strings.Contains(resp.Body.String(), "pull disabled") {
			t.Fatalf("expected stale-cache error message, got %q", resp.Body.String())
		}
	})

	t.Run("status node missing cache with pull disabled", func(t *testing.T) {
		h := newHealth()
		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/node-missing", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusNotFound {
			t.Fatalf("expected 404 when no cached status and pull disabled, got %d", resp.Code)
		}
	})

	t.Run("status node live pull requires node informer", func(t *testing.T) {
		h := newHealth()
		h.pullEnabled.Store(true)

		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/node-a?live=true", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 when node informer not ready, got %d", resp.Code)
		}
	})

	t.Run("status node live pull returns not found when node missing", func(t *testing.T) {
		h := newHealth()
		h.pullEnabled.Store(true)

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		h.nodeLister = corev1listers.NewNodeLister(indexer)

		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/node-a?live=true", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusNotFound {
			t.Fatalf("expected 404 when node is not present in lister, got %d", resp.Code)
		}
	})

	t.Run("status node live pull returns internal error when internal ip missing", func(t *testing.T) {
		h := newHealth()
		h.pullEnabled.Store(true)

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
		if err := indexer.Add(node); err != nil {
			t.Fatalf("failed to add node to indexer: %v", err)
		}

		h.nodeLister = corev1listers.NewNodeLister(indexer)

		mux := http.NewServeMux()
		registerStatusHandlers(mux, h, false, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/status/node/node-a?live=true", nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when node has no InternalIP, got %d", resp.Code)
		}
	})
}

// TestRegisterProbeHandlers tests RegisterProbeHandlers.
func TestRegisterProbeHandlers(t *testing.T) {
	mux := http.NewServeMux()
	health := &healthState{
		clientset: k8sfake.NewClientset(),
		tokenAuth: &tokenAuthenticator{tokenReviewer: k8sfake.NewClientset()},
	}
	registerProbeHandlers(mux, health)

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResp := httptest.NewRecorder()
	mux.ServeHTTP(healthResp, healthReq)

	if healthResp.Code != http.StatusOK {
		t.Fatalf("expected /healthz to return 200, got %d body=%q", healthResp.Code, healthResp.Body.String())
	}

	readyReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	readyResp := httptest.NewRecorder()
	mux.ServeHTTP(readyResp, readyReq)

	if readyResp.Code != http.StatusOK {
		t.Fatalf("expected /readyz to return 200, got %d body=%q", readyResp.Code, readyResp.Body.String())
	}
}

// TestRegisterProbeHandlersTokenVerifierNotReady tests RegisterProbeHandlersTokenVerifierNotReady.
func TestRegisterProbeHandlersTokenVerifierNotReady(t *testing.T) {
	mux := http.NewServeMux()
	health := &healthState{
		clientset: k8sfake.NewClientset(),
		tokenAuth: &tokenAuthenticator{},
	}
	registerProbeHandlers(mux, health)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /readyz to return 503 when token verifier is unavailable, got %d", resp.Code)
	}

	if !strings.Contains(resp.Body.String(), "token verifier not ready") {
		t.Fatalf("expected readyz body to mention token verifier readiness, got %q", resp.Body.String())
	}
}

// TestRegisterPushHandlers tests RegisterPushHandlers.
func TestRegisterPushHandlers(t *testing.T) {
	// testWebhookServerForPush creates a webhook.Server whose IsTrustedAggregatedRequest
	// returns true when the request has the expected client certificate.
	testWebhookServerForPush := func(t *testing.T, caPEM []byte) *webhook.Server {
		t.Helper()

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "extension-apiserver-authentication", Namespace: "kube-system"},
			Data: map[string]string{
				"requestheader-client-ca-file": string(caPEM),
			},
		}
		clientset := k8sfake.NewClientset(cm)
		ws := webhook.NewTestServer(clientset, "kube-system")
		// Force refresh of aggregated client CAs from the fake ConfigMap.
		ws.RefreshAggregatedClientCAs(t.Context())

		return ws
	}

	issuer := testTokenIssuer(t)
	validToken := testNodeToken(t, issuer)

	newHealth := func() *healthState {
		h := &healthState{
			statusCache:                 NewNodeStatusCache(),
			nodeServiceAccount:          "unbounded-net:unbounded-net-node",
			registerAggregatedAPIServer: true,
			tokenAuth: &tokenAuthenticator{
				cache:    map[string]*tokenAuthResult{},
				cacheTTL: time.Minute,
			},
		}
		h.isLeader.Store(true)

		return h
	}

	// Generate a client cert for front-proxy auth tests.
	clientCertPEM, _, caPEM, err := webhook.GenerateClientAuthCertificateForTest("front-proxy-client")
	if err != nil {
		t.Fatalf("generateClientAuthCertificate: %v", err)
	}

	clientCert, err := x509.ParseCertificate(mustParseCertPEM(t, clientCertPEM))
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	t.Run("method not allowed", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		req := httptest.NewRequest(http.MethodGet, "/status/push", nil)
		req.Header.Set("Authorization", "Bearer "+validToken)

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405 for GET /status/push, got %d", resp.Code)
		}
	})

	t.Run("not leader", func(t *testing.T) {
		h := newHealth()
		h.isLeader.Store(false)

		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		req := httptest.NewRequest(http.MethodPost, "/status/push", bytes.NewBufferString(`{}`))
		req.Header.Set("Authorization", "Bearer "+validToken)

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 when not leader, got %d", resp.Code)
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		req := httptest.NewRequest(http.MethodPost, "/status/push", bytes.NewBufferString(`{}`))
		req.Header.Set("Authorization", "Bearer invalid-token")

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthorized token, got %d", resp.Code)
		}
	})

	t.Run("gzip decode failure", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		req := httptest.NewRequest(http.MethodPost, "/status/push", bytes.NewBufferString("not-gzip"))
		req.Header.Set("Authorization", "Bearer "+validToken)
		req.Header.Set("Content-Encoding", "gzip")

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid gzip body, got %d", resp.Code)
		}
	})

	t.Run("full push success", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		payload := `{"mode":"full","nodeName":"node-a","status":{"nodeInfo":{"siteName":"site-a"}}}`
		req := httptest.NewRequest(http.MethodPost, "/status/push", bytes.NewBufferString(payload))
		req.Header.Set("Authorization", "Bearer "+validToken)

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200 for successful full push, got %d body=%q", resp.Code, resp.Body.String())
		}

		var ack NodeStatusPushAck
		if err := json.Unmarshal(resp.Body.Bytes(), &ack); err != nil {
			t.Fatalf("failed to decode push ack: %v", err)
		}

		if ack.Status != "ok" || ack.Revision == 0 {
			t.Fatalf("unexpected push ack: %#v", ack)
		}
	})

	t.Run("full push success with gzip", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		var body bytes.Buffer

		gz := gzip.NewWriter(&body)
		_, _ = gz.Write([]byte(`{"mode":"full","nodeName":"node-b","status":{"nodeInfo":{"siteName":"site-b"}}}`))
		_ = gz.Close()

		req := httptest.NewRequest(http.MethodPost, "/status/push", &body)
		req.Header.Set("Authorization", "Bearer "+validToken)
		req.Header.Set("Content-Encoding", "gzip")

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200 for successful gzip full push, got %d body=%q", resp.Code, resp.Body.String())
		}
	})

	t.Run("node websocket endpoint requires leader", func(t *testing.T) {
		h := newHealth()
		h.isLeader.Store(false)

		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		req := httptest.NewRequest(http.MethodGet, "/status/nodews", nil)
		req.Header.Set("Authorization", "Bearer "+validToken)

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for /status/nodews when not leader, got %d", resp.Code)
		}
	})

	t.Run("node websocket endpoint requires authorization", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		req := httptest.NewRequest(http.MethodGet, "/status/nodews", nil)
		req.Header.Set("Authorization", "Bearer invalid-token")

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for /status/nodews with invalid token, got %d", resp.Code)
		}
	})

	t.Run("aggregated push with valid front-proxy cert", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		payload := `{"mode":"full","nodeName":"node-a","status":{"nodeInfo":{"siteName":"site-a"}}}`
		req := httptest.NewRequest(http.MethodPost, aggregatedNodeStatusPushPath, bytes.NewBufferString(payload))
		req.Header.Set("X-Remote-User", "system:serviceaccount:unbounded-net:unbounded-net-node")
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{clientCert}}
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200 for aggregated push with front-proxy cert, got %d body=%q", resp.Code, resp.Body.String())
		}
	})

	t.Run("aggregated push rejects unexpected X-Remote-User", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		payload := `{"mode":"full","nodeName":"node-a","status":{"nodeInfo":{"siteName":"site-a"}}}`
		req := httptest.NewRequest(http.MethodPost, aggregatedNodeStatusPushPath, bytes.NewBufferString(payload))
		req.Header.Set("X-Remote-User", "system:serviceaccount:default:other-sa")
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{clientCert}}
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for aggregated push with wrong user, got %d", resp.Code)
		}
	})

	t.Run("aggregated push rejects without client cert", func(t *testing.T) {
		h := newHealth()
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		payload := `{"mode":"full","nodeName":"node-a","status":{"nodeInfo":{"siteName":"site-a"}}}`
		req := httptest.NewRequest(http.MethodPost, aggregatedNodeStatusPushPath, bytes.NewBufferString(payload))
		req.Header.Set("X-Remote-User", "system:serviceaccount:unbounded-net:unbounded-net-node")

		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)

		if resp.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without client cert, got %d", resp.Code)
		}
	})

	t.Run("aggregated endpoints are not registered when disabled", func(t *testing.T) {
		h := newHealth()
		h.registerAggregatedAPIServer = false
		ws := testWebhookServerForPush(t, caPEM)
		mux := http.NewServeMux()
		wsSem := make(chan struct{}, maxConcurrentNodeWS)
		registerPushHandlers(mux, h, ws, wsSem, issuer)

		wsReq := httptest.NewRequest(http.MethodGet, aggregatedNodeStatusWebSocketPath, nil)
		wsResp := httptest.NewRecorder()
		mux.ServeHTTP(wsResp, wsReq)

		if wsResp.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for disabled aggregated websocket route, got %d", wsResp.Code)
		}

		pushReq := httptest.NewRequest(http.MethodPost, aggregatedNodeStatusPushPath, bytes.NewBufferString(`{}`))
		pushResp := httptest.NewRecorder()
		mux.ServeHTTP(pushResp, pushReq)

		if pushResp.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for disabled aggregated push route, got %d", pushResp.Code)
		}
	})
}

func mustParseCertPEM(t *testing.T, certPEM []byte) []byte {
	t.Helper()

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode PEM cert")
	}

	return block.Bytes
}
