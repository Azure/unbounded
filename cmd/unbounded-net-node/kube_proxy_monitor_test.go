// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKubeProxyMonitorHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	m := newKubeProxyMonitor(100 * time.Millisecond)
	m.url = srv.URL + "/healthz"
	m.check()

	if w := m.GetWarning(); w != "" {
		t.Fatalf("expected no warning, got: %s", w)
	}
}

func TestKubeProxyMonitorUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	}))
	defer srv.Close()

	m := newKubeProxyMonitor(100 * time.Millisecond)
	m.url = srv.URL + "/healthz"
	m.check()

	w := m.GetWarning()
	if w == "" {
		t.Fatal("expected warning, got empty")
	}

	if w != "kube-proxy healthz returned 503: not ready" {
		t.Fatalf("unexpected warning: %s", w)
	}
}

func TestKubeProxyMonitorUnreachable(t *testing.T) {
	m := newKubeProxyMonitor(100 * time.Millisecond)
	m.url = "http://127.0.0.1:1/healthz" // nothing listening
	m.check()

	w := m.GetWarning()
	if w == "" {
		t.Fatal("expected warning for unreachable endpoint")
	}
}

func TestKubeProxyMonitorRecovery(t *testing.T) {
	healthy := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	m := newKubeProxyMonitor(100 * time.Millisecond)
	m.url = srv.URL + "/healthz"

	// Start healthy
	m.check()

	if m.GetWarning() != "" {
		t.Fatal("expected no warning when healthy")
	}

	// Go unhealthy
	healthy = false

	m.check()

	if m.GetWarning() == "" {
		t.Fatal("expected warning when unhealthy")
	}

	// Recover
	healthy = true

	m.check()

	if m.GetWarning() != "" {
		t.Fatal("expected warning to clear on recovery")
	}
}

func TestKubeProxyMonitorDisabledWithZeroInterval(t *testing.T) {
	m := newKubeProxyMonitor(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.Start(ctx)
		close(done)
	}()

	// Start should return immediately when interval is 0
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return for disabled monitor")
	}

	if m.GetWarning() != "" {
		t.Fatal("disabled monitor should have no warning")
	}
}
