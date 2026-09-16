// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func consumeTestWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	for {
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
	}
}

func TestWebSocketFallbackIndependentOfHTTP(t *testing.T) {
	for _, pushEnabled := range []bool{false, true} {
		name := "HTTP disabled"
		if pushEnabled {
			name = "HTTP recovered"
		}

		t.Run(name, func(t *testing.T) {
			var directCalls, fallbackCalls atomic.Int32

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/status/nodews" {
					directCalls.Add(1)
					http.Error(w, "direct websocket unavailable", http.StatusServiceUnavailable)

					return
				}

				fallbackCalls.Add(1)
				consumeTestWebSocket(w, r)
			}))
			defer server.Close()

			host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
			if err != nil {
				t.Fatal(err)
			}

			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", host)
			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", port)

			cfg := &config{
				NodeName: "node-a", StatusWSEnabled: true, StatusPushEnabled: pushEnabled,
				StatusWSAPIServerMode:         statusWSAPIServerModeFallback,
				StatusWSAPIServerURL:          "wss" + strings.TrimPrefix(server.URL, "https") + "/apis/status/nodews",
				StatusWSAPIServerStartupDelay: 25 * time.Millisecond,
			}
			manager := &hmacTokenManager{token: "valid-token", issuedAt: time.Now(), expiresAt: time.Now().Add(time.Hour)}

			var (
				connected, fallbackEnabled, apiEnabled, closeFallback atomic.Bool
				mode                                                  atomic.Int32
			)

			ctx, cancel := context.WithCancel(t.Context())

			done := make(chan struct{})
			go func() {
				defer close(done)

				runStatusWebSocketPusher(ctx, cfg, blockedBootstrapHealthState(), &connected, &mode,
					&fallbackEnabled, &apiEnabled, &closeFallback, server.Client(), manager)
			}()

			defer func() { cancel(); <-done }()

			waitForStatusCondition(t, func() bool { return mode.Load() == statusWSModeFallback })

			if directCalls.Load() == 0 || fallbackCalls.Load() != 1 {
				t.Fatalf("expected direct failure then fallback, got direct=%d fallback=%d", directCalls.Load(), fallbackCalls.Load())
			}

			if pushEnabled {
				// HTTP recovery must not tear down a working fallback while direct WS is down.
				closeFallback.Store(true)
				waitForStatusCondition(t, func() bool { return !closeFallback.Load() })

				if mode.Load() != statusWSModeFallback || fallbackCalls.Load() != 1 {
					t.Fatal("HTTP recovery displaced fallback without direct WebSocket recovery")
				}
			}
		})
	}
}

func waitForStatusCondition(t *testing.T, condition func() bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for status transport")
		case <-ticker.C:
		}
	}
}

func TestWebSocketUnauthorizedRefreshesToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("service-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	var exchanges, rejected atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			_ = json.NewEncoder(w).Encode(hmacTokenResponse{Token: "fresh-token", NodeName: "node-a", ExpiresAt: time.Now().Add(time.Hour)})

			return
		}

		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			rejected.Add(1)
			http.Error(w, "expired credential", http.StatusUnauthorized)

			return
		}

		consumeTestWebSocket(w, r)
	}))
	defer server.Close()

	manager := &hmacTokenManager{
		nodeName: "node-a", token: "rejected-token", issuedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
		saTokenPath: tokenPath, tokenURLs: []string{server.URL + "/token"}, client: server.Client(),
	}
	cfg := &config{NodeName: "node-a", StatusWSEnabled: true, StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/direct"}

	var mode atomic.Int32

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)

		runStatusWebSocketPusher(ctx, cfg, blockedBootstrapHealthState(), nil, &mode, nil, nil, nil, server.Client(), manager)
	}()

	defer func() { cancel(); <-done }()

	waitForStatusCondition(t, func() bool { return mode.Load() == statusWSModeDirect })

	if exchanges.Load() != 1 || rejected.Load() != 1 {
		t.Fatalf("expected one rejected dial and fresh exchange, got rejected=%d exchanges=%d", rejected.Load(), exchanges.Load())
	}
}

func TestDirectRecoveryUnauthorizedInvalidatesToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired credential", http.StatusUnauthorized)
	}))
	defer server.Close()

	invalidated := false
	if tryDirectRecoveryProbe(t.Context(), &nodeHealthState{}, server.Client(), func() string { return "token" },
		func() { invalidated = true }, "ws"+strings.TrimPrefix(server.URL, "http"), "node-a") || !invalidated {
		t.Fatal("unauthorized recovery probe did not invalidate the credential")
	}
}
