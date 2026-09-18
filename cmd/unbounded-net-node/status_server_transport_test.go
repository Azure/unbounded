// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPStatusTransportDirectFirst(t *testing.T) {
	t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	for _, mode := range []string{statusWSAPIServerModeFallback, statusWSAPIServerModePreferred, statusWSAPIServerModeNever} {
		t.Run(mode, func(t *testing.T) {
			tests := []struct {
				name          string
				directStatus  int
				apiStatus     int
				recoverDirect bool
				noDirect      bool
				startupDelay  time.Duration
				apiInterval   time.Duration
				wsMode        int32
				wantRequests  []string
				wantNever     []string
			}{
				{
					name:         "healthy direct wins even when fallback is eligible",
					directStatus: http.StatusOK,
					wantRequests: []string{"direct", "direct"},
				},
				{
					name:          "direct recovers after API server fallback",
					directStatus:  http.StatusServiceUnavailable,
					recoverDirect: true,
					wantRequests:  []string{"direct", "apiserver", "direct", "direct"},
					wantNever:     []string{"direct", "direct", "direct"},
				},
				{
					name:         "both endpoints fail",
					directStatus: http.StatusServiceUnavailable,
					apiStatus:    http.StatusServiceUnavailable,
					wantRequests: []string{"direct", "apiserver", "direct", "apiserver"},
					wantNever:    []string{"direct", "direct"},
				},
				{
					name:         "direct resync does not trigger fallback",
					directStatus: http.StatusTooManyRequests,
					wantRequests: []string{"direct", "direct"},
				},
				{
					name:         "startup delay suppresses fallback",
					directStatus: http.StatusServiceUnavailable,
					startupDelay: time.Hour,
					wantRequests: []string{"direct", "direct"},
				},
				{
					name:         "active fallback websocket suppresses API push",
					directStatus: http.StatusServiceUnavailable,
					wsMode:       statusWSModeFallback,
					wantRequests: []string{"direct", "direct"},
				},
				{
					name:         "API push interval throttles fallback",
					directStatus: http.StatusServiceUnavailable,
					apiInterval:  time.Hour,
					wantRequests: []string{"direct", "apiserver", "direct", "direct"},
					wantNever:    []string{"direct", "direct", "direct"},
				},
				{
					name:         "no direct path uses API fallback without startup delay",
					noDirect:     true,
					startupDelay: time.Hour,
					wantRequests: []string{"apiserver", "apiserver"},
					wantNever:    []string{},
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					requests := make(chan string, 32)

					var directRequests atomic.Int32

					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)

						target := "apiserver"
						status := tt.apiStatus

						if r.URL.Path == "/direct" {
							target = "direct"

							status = tt.directStatus
							if directRequests.Add(1) > 1 && tt.recoverDirect {
								status = http.StatusOK
							}
						}

						select {
						case requests <- target:
						case <-r.Context().Done():
							return
						}

						if status == 0 {
							status = http.StatusOK
						}

						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"status":"ok","revision":1}`)
					}))
					defer server.Close()

					cfg := &config{
						NodeName:                      "node-a",
						StatusPushEnabled:             true,
						StatusPushURL:                 server.URL + "/direct",
						StatusPushInterval:            10 * time.Millisecond,
						StatusPushAPIServerInterval:   time.Millisecond,
						StatusWSAPIServerMode:         mode,
						StatusWSAPIServerURL:          "ws" + strings.TrimPrefix(server.URL, "http") + "/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews",
						StatusWSAPIServerStartupDelay: tt.startupDelay,
					}
					if tt.noDirect {
						cfg.StatusPushURL = ""
					}

					if tt.apiInterval > 0 {
						cfg.StatusPushAPIServerInterval = tt.apiInterval
					}

					var wsMode atomic.Int32
					wsMode.Store(tt.wsMode)

					var fallbackEnabled, apiEnabled, closeFallback atomic.Bool
					fallbackEnabled.Store(true)
					apiEnabled.Store(true)

					ctx, cancel := context.WithCancel(context.Background())
					done := make(chan struct{})

					go func() {
						defer close(done)

						startStatusPusher(ctx, cfg, blockedBootstrapHealthState(), nil, &wsMode, &fallbackEnabled, &apiEnabled, &closeFallback)
					}()

					defer func() {
						cancel()
						<-done
					}()

					want := tt.wantRequests
					if mode == statusWSAPIServerModeNever && tt.wantNever != nil {
						want = tt.wantNever
					}

					timeout := time.After(3 * time.Second)

					for _, expected := range want {
						select {
						case got := <-requests:
							if got != expected {
								t.Fatalf("request target = %q, want %q (sequence %v)", got, expected, want)
							}
						case <-timeout:
							t.Fatalf("timed out waiting for %q (sequence %v)", expected, want)
						}
					}

					if len(want) == 0 {
						select {
						case got := <-requests:
							t.Fatalf("unexpected request to %q with API mode never and no direct endpoint", got)
						case <-done:
						case <-timeout:
							t.Fatal("publisher did not stop without a permitted endpoint")
						}
					}
				})
			}
		})
	}
}
