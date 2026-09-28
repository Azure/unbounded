// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

type establishedWriteFailureTransport struct {
	base         http.RoundTripper
	fail         *atomic.Bool
	failedWrites *atomic.Int32
}

func (transport establishedWriteFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(req)
	if err == nil && req.URL.Path == "/status/nodews" && response.StatusCode == http.StatusSwitchingProtocols {
		response.Body = establishedWriteFailureBody{
			ReadWriteCloser: response.Body.(io.ReadWriteCloser),
			fail:            transport.fail,
			failedWrites:    transport.failedWrites,
		}
	}

	return response, err
}

type establishedWriteFailureBody struct {
	io.ReadWriteCloser
	fail         *atomic.Bool
	failedWrites *atomic.Int32
}

func (body establishedWriteFailureBody) Write(data []byte) (int, error) {
	if body.fail.Load() {
		body.failedWrites.Add(1)

		return 0, errors.New("injected established status write failure")
	}

	return body.ReadWriteCloser.Write(data)
}

func TestWebSocketEstablishedFailureFallsBack(t *testing.T) {
	for _, mode := range []string{statusWSAPIServerModeFallback, statusWSAPIServerModePreferred, statusWSAPIServerModeNever} {
		for _, failure := range []string{"read", "full sync"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				var (
					directCalls, fallbackCalls, failedWrites atomic.Int32
					connected, failWrites                    atomic.Bool
					wsMode                                   atomic.Int32
				)

				dropDirect := make(chan struct{})
				initialStatus := make(chan []byte, 1)

				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/status/nodews" {
						fallbackCalls.Add(1)
						consumeTestWebSocket(w, r)

						return
					}

					if directCalls.Add(1) > 1 {
						consumeTestWebSocket(w, r)

						return
					}

					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.CloseNow() }()

					_, data, err := conn.Read(ctx)
					if err != nil {
						return
					}

					initialStatus <- data

					ack, err := proto.Marshal(&statusproto.NodeStatusAck{Status: "ok", Revision: 1})
					if err != nil {
						t.Error(err)
						return
					}

					if err := conn.Write(ctx, websocket.MessageBinary, ack); err != nil {
						return
					}

					if failure == "read" {
						select {
						case <-dropDirect:
						case <-ctx.Done():
						}

						return
					}

					for {
						if _, _, err := conn.Read(ctx); err != nil {
							return
						}
					}
				}))
				defer server.Close()

				host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
				if err != nil {
					t.Fatal(err)
				}

				t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", host)
				t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", port)

				cfg := &config{
					NodeName: "node-a", StatusWSEnabled: true, StatusPushEnabled: false,
					StatusWSAPIServerMode:         mode,
					StatusWSAPIServerURL:          "wss" + strings.TrimPrefix(server.URL, "https") + "/apis/status/nodews",
					StatusWSAPIServerStartupDelay: 25 * time.Millisecond,
					FullSyncEvery:                 20 * time.Millisecond,
				}
				client := server.Client()
				client.Transport = establishedWriteFailureTransport{
					base: client.Transport, fail: &failWrites, failedWrites: &failedWrites,
				}
				manager := &hmacTokenManager{token: "valid-token", issuedAt: time.Now(), expiresAt: time.Now().Add(time.Hour)}
				done := make(chan struct{})

				go func() {
					defer close(done)

					runStatusWebSocketPusher(ctx, cfg, blockedBootstrapHealthState(), &connected, &wsMode,
						nil, nil, nil, client, manager)
				}()

				defer func() { cancel(); <-done }()

				waitForStatusCondition(t, func() bool { return connected.Load() && wsMode.Load() == statusWSModeDirect })

				select {
				case data := <-initialStatus:
					var message statusproto.NodeStatusMessage
					if err := proto.Unmarshal(data, &message); err != nil {
						t.Fatal(err)
					}

					if message.Type != "node_status_full" || message.NodeName != "node-a" || message.Status == nil {
						t.Fatalf("direct session did not deliver its initial full status: %v", &message)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("direct session did not deliver its initial full status")
				}

				if failure == "read" {
					close(dropDirect)
				} else {
					failWrites.Store(true)
				}

				if mode == statusWSAPIServerModeNever {
					waitForStatusCondition(t, func() bool { return directCalls.Load() > 1 })

					if fallbackCalls.Load() != 0 {
						t.Fatal("established direct failure enabled forbidden fallback")
					}
				} else {
					waitForStatusCondition(t, func() bool { return connected.Load() && wsMode.Load() == statusWSModeFallback })

					if directCalls.Load() != 1 || fallbackCalls.Load() != 1 {
						t.Fatalf("expected fallback before retrying broken direct session, got direct=%d fallback=%d",
							directCalls.Load(), fallbackCalls.Load())
					}
				}

				if failure == "full sync" && failedWrites.Load() == 0 {
					t.Fatal("established session never attempted the failing full sync write")
				}
			})
		}
	}
}

func TestWebSocketEstablishedShutdownDoesNotReconnect(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		consumeTestWebSocket(w, r)
	}))
	defer server.Close()

	cfg := &config{
		NodeName: "node-a", StatusWSEnabled: true, StatusPushEnabled: false,
		StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/direct",
	}
	manager := &hmacTokenManager{token: "valid-token", issuedAt: time.Now(), expiresAt: time.Now().Add(time.Hour)}

	var (
		connected atomic.Bool
		mode      atomic.Int32
	)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		runStatusWebSocketPusher(ctx, cfg, blockedBootstrapHealthState(), &connected, &mode,
			nil, nil, nil, server.Client(), manager)
	}()

	defer func() { cancel(); <-done }()

	waitForStatusCondition(t, func() bool { return connected.Load() && mode.Load() == statusWSModeDirect })
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("established session did not stop on cancellation")
	}

	if connected.Load() || mode.Load() != statusWSModeNone || calls.Load() != 1 {
		t.Fatal("shutdown retained a transport or attempted a reconnect")
	}
}

func TestWebSocketRecoveryPromotesInitializedConnection(t *testing.T) {
	for _, testCase := range []string{"timer/full", "HTTP recovery/full", "timer/summary", "HTTP recovery/summary"} {
		t.Run(testCase, func(t *testing.T) {
			trigger, publicationMode, _ := strings.Cut(testCase, "/")

			var (
				directCalls, fallbackCalls, failedWrites atomic.Int32
				directFrames, fallbackFrames             atomic.Int32
				connected, failWrites, fallbackClosed    atomic.Bool
				closeFallback                            atomic.Bool
				initialAckSent                           atomic.Bool
				mode                                     atomic.Int32
				detailCollections                        atomic.Int32
			)

			failWrites.Store(true)

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				direct := r.URL.Path == "/status/nodews"
				if direct {
					directCalls.Add(1)
				} else {
					fallbackCalls.Add(1)

					defer fallbackClosed.Store(true)
				}

				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()

				for {
					_, data, err := conn.Read(r.Context())
					if err != nil {
						return
					}

					var message statusproto.NodeStatusMessage
					if err := proto.Unmarshal(data, &message); err != nil {
						t.Errorf("invalid status frame: %v", err)
						return
					}

					if message.NodeName != "node-a" {
						t.Errorf("unexpected status identity %q", message.NodeName)
						return
					}

					if !message.SupportsDetails || (publicationMode == "summary" && (message.Summary == nil || message.Status != nil || message.Delta != nil)) {
						t.Error("recovery changed publication mode or lost detail capability")
						return
					}

					var revision int32
					if direct {
						revision = directFrames.Add(1)
						if revision > 1 && !initialAckSent.Load() {
							t.Error("promoted connection published before its initial ACK")
							return
						}
					} else {
						revision = fallbackFrames.Add(1)
					}

					ack, err := proto.Marshal(&statusproto.NodeStatusAck{Status: "ok", Revision: uint64(revision), SummarySupported: true})
					if err != nil {
						t.Error(err)
						return
					}

					if direct && revision == 1 {
						go func() {
							select {
							case <-r.Context().Done():
								return
							case <-time.After(150 * time.Millisecond):
							}

							initialAckSent.Store(true)

							if err := conn.Write(r.Context(), websocket.MessageBinary, ack); err != nil && r.Context().Err() == nil {
								t.Errorf("initial recovery ACK failed: %v", err)
							}
						}()

						continue
					}

					if err := conn.Write(r.Context(), websocket.MessageBinary, ack); err != nil {
						return
					}
				}
			}))
			defer server.Close()

			host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
			if err != nil {
				t.Fatal(err)
			}

			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", host)
			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", port)

			cfg := &config{
				NodeName: "node-a", StatusWSEnabled: true, StatusDetailMode: publicationMode,
				StatusWSAPIServerMode:         statusWSAPIServerModeFallback,
				StatusWSAPIServerURL:          "wss" + strings.TrimPrefix(server.URL, "https") + "/apis/status/nodews",
				StatusWSAPIServerStartupDelay: 25 * time.Millisecond,
				FullSyncEvery:                 20 * time.Millisecond,
			}
			client := server.Client()
			client.Transport = establishedWriteFailureTransport{
				base: client.Transport, fail: &failWrites, failedWrites: &failedWrites,
			}
			manager := &hmacTokenManager{token: "valid-token", issuedAt: time.Now(), expiresAt: time.Now().Add(time.Hour)}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			health := blockedBootstrapHealthState()

			if publicationMode == "summary" {
				statusServer := summaryRouteFixture()
				statusServer.cfg.NodeName = "node-a"
				statusServer.bpfCollector = func() []BpfEntry {
					detailCollections.Add(1)
					return nil
				}
				health.setStatusServer(statusServer)
			}

			go func() {
				defer close(done)

				runStatusWebSocketPusher(ctx, cfg, health, &connected, &mode,
					nil, nil, &closeFallback, client, manager)
			}()

			defer func() { cancel(); <-done }()

			waitForStatusCondition(t, func() bool { return mode.Load() == statusWSModeFallback })

			if trigger == "HTTP recovery" {
				closeFallback.Store(true)
			}

			waitForStatusCondition(t, func() bool { return failedWrites.Load() >= 2 })

			frames := fallbackFrames.Load()

			waitForStatusCondition(t, func() bool { return fallbackFrames.Load() > frames })

			if fallbackClosed.Load() || fallbackCalls.Load() != 1 || !connected.Load() || mode.Load() != statusWSModeFallback {
				t.Fatal("failed recovery initial write displaced the working fallback")
			}

			failWrites.Store(false)

			if trigger == "HTTP recovery" {
				closeFallback.Store(true)
			}

			waitForStatusCondition(t, func() bool {
				return mode.Load() == statusWSModeDirect && directFrames.Load() >= 2
			})

			if directCalls.Load() != 3 || fallbackCalls.Load() != 1 || !fallbackClosed.Load() {
				t.Fatalf("recovery must promote its connection without redial: direct=%d fallback=%d fallbackClosed=%v",
					directCalls.Load(), fallbackCalls.Load(), fallbackClosed.Load())
			}

			if detailCollections.Load() != 0 {
				t.Fatal("summary recovery collected full diagnostics")
			}
		})
	}
}

type initialWriteFailureTransport struct {
	base      http.RoundTripper
	failed    *atomic.Int32
	connected *atomic.Bool
	mode      *atomic.Int32
	premature *atomic.Bool
}

func (transport initialWriteFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(req)
	if err == nil && req.URL.Path == "/status/nodews" && response.StatusCode == http.StatusSwitchingProtocols {
		response.Body = initialWriteFailureBody{
			ReadCloser: response.Body, transport: transport,
		}
	}

	return response, err
}

type initialWriteFailureBody struct {
	io.ReadCloser
	transport initialWriteFailureTransport
}

func (body initialWriteFailureBody) Write([]byte) (int, error) {
	if body.transport.connected.Load() || body.transport.mode.Load() == statusWSModeDirect {
		body.transport.premature.Store(true)
	}

	body.transport.failed.Add(1)

	return 0, errors.New("injected first status write failure after successful handshake")
}

func TestWebSocketInitialWriteFailureFallsBack(t *testing.T) {
	for _, mode := range []string{statusWSAPIServerModeFallback, statusWSAPIServerModePreferred, statusWSAPIServerModeNever} {
		t.Run(mode, func(t *testing.T) {
			var (
				fallbackCalls, failedWrites atomic.Int32
				connected, premature        atomic.Bool
				wsMode                      atomic.Int32
			)

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status/nodews" {
					fallbackCalls.Add(1)
				}

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
				NodeName: "node-a", StatusWSEnabled: true, StatusPushEnabled: false,
				StatusWSAPIServerMode:         mode,
				StatusWSAPIServerURL:          "wss" + strings.TrimPrefix(server.URL, "https") + "/apis/status/nodews",
				StatusWSAPIServerStartupDelay: 25 * time.Millisecond,
			}
			client := server.Client()
			client.Transport = initialWriteFailureTransport{
				base: client.Transport, failed: &failedWrites,
				connected: &connected, mode: &wsMode, premature: &premature,
			}
			manager := &hmacTokenManager{token: "valid-token", issuedAt: time.Now(), expiresAt: time.Now().Add(time.Hour)}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})

			go func() {
				defer close(done)

				runStatusWebSocketPusher(ctx, cfg, blockedBootstrapHealthState(), &connected, &wsMode,
					nil, nil, nil, client, manager)
			}()

			defer func() { cancel(); <-done }()

			waitForStatusCondition(t, func() bool { return failedWrites.Load() > 0 })

			if mode == statusWSAPIServerModeNever {
				time.Sleep(100 * time.Millisecond)

				if fallbackCalls.Load() != 0 || connected.Load() || wsMode.Load() != statusWSModeNone {
					t.Fatal("failed initial write enabled fallback or advertised a usable connection in never mode")
				}
			} else {
				waitForStatusCondition(t, func() bool { return connected.Load() && wsMode.Load() == statusWSModeFallback })

				if fallbackCalls.Load() == 0 {
					t.Fatal("initial write failure did not attempt the working fallback")
				}
			}

			if premature.Load() {
				t.Fatal("direct transport was advertised before its initial status write succeeded")
			}
		})
	}
}

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
		func() { invalidated = true }, "ws"+strings.TrimPrefix(server.URL, "http"), "node-a", "full") != nil || !invalidated {
		t.Fatal("unauthorized recovery probe did not invalidate the credential")
	}
}
