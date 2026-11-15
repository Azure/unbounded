// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAPIServerPushFailureRecordsFallbackPush drives the real status push loop
// with both endpoints failing and asserts the aggregated API failure is
// reported as a fallback push error instead of being dropped.
func TestAPIServerPushFailureRecordsFallbackPush(t *testing.T) {
	t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	requests := make(chan string, 8)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/direct"):
			select {
			case requests <- "direct":
			default:
			}
		case strings.HasSuffix(r.URL.Path, "/status/push"):
			select {
			case requests <- "apiserver":
			default:
			}
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := &config{
		NodeName:                    "node-a",
		StatusPushEnabled:           true,
		StatusPushURL:               server.URL + "/direct",
		StatusPushInterval:          10 * time.Millisecond,
		StatusPushAPIServerInterval: time.Millisecond,
		StatusWSAPIServerMode:       statusWSAPIServerModeFallback,
		StatusWSAPIServerURL:        "ws" + strings.TrimPrefix(server.URL, "http") + "/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews",
	}

	health := blockedBootstrapHealthState()

	var wsMode atomic.Int32

	var fallbackEnabled, apiEnabled, closeFallback atomic.Bool
	fallbackEnabled.Store(true)
	apiEnabled.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		startStatusPusher(ctx, cfg, health, nil, &wsMode, &fallbackEnabled, &apiEnabled, &closeFallback)
	}()

	deadline := time.After(5 * time.Second)

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case target := <-requests:
			seen[target] = true
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for both push targets, saw %v", seen)
		}
	}

	cancel()
	<-done

	health.mu.RLock()
	defer health.mu.RUnlock()

	for _, nodeError := range health.transientErrors {
		if nodeError.Type == nodeErrorTypeFallbackPush {
			return
		}
	}

	t.Fatalf("expected a %s error after the aggregated API push failed, got %#v", nodeErrorTypeFallbackPush, health.transientErrors)
}
