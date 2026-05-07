// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// kubeProxyMonitor periodically checks the kube-proxy healthz endpoint
// and surfaces a warning when the check fails.
type kubeProxyMonitor struct {
	interval time.Duration
	url      string
	client   *http.Client

	mu      sync.Mutex
	warning string // empty when healthy
}

// newKubeProxyMonitor creates a monitor that checks kube-proxy health at
// the given interval. An interval of 0 disables the monitor.
func newKubeProxyMonitor(interval time.Duration) *kubeProxyMonitor {
	return &kubeProxyMonitor{
		interval: interval,
		url:      "http://127.0.0.1:10256/healthz",
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Start runs the monitor loop until the context is cancelled.
// If the interval is 0, the monitor does nothing.
func (m *kubeProxyMonitor) Start(ctx context.Context) {
	if m.interval <= 0 {
		klog.V(2).Info("kube-proxy health monitor disabled (interval=0)")
		return
	}

	klog.V(2).Infof("kube-proxy health monitor started (interval %s, url %s)", m.interval, m.url)

	// Check immediately on startup so the warning is available before the first tick.
	m.check()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(2).Info("kube-proxy health monitor stopped")
			return
		case <-ticker.C:
			m.check()
		}
	}
}

// GetWarning returns the current warning message, or empty string if healthy.
func (m *kubeProxyMonitor) GetWarning() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.warning
}

// check probes the kube-proxy healthz endpoint and updates the warning state.
func (m *kubeProxyMonitor) check() {
	resp, err := m.client.Get(m.url)
	if err != nil {
		m.setWarning(fmt.Sprintf("kube-proxy healthz unreachable: %v", err))
		return
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256)) //nolint:errcheck

	if resp.StatusCode == http.StatusOK {
		m.clearWarning()
		return
	}

	m.setWarning(fmt.Sprintf("kube-proxy healthz returned %d: %s", resp.StatusCode, string(body)))
}

func (m *kubeProxyMonitor) setWarning(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.warning != msg {
		klog.V(2).Infof("kube-proxy health warning: %s", msg)
	}

	m.warning = msg
}

func (m *kubeProxyMonitor) clearWarning() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.warning != "" {
		klog.V(2).Info("kube-proxy health check recovered")
	}

	m.warning = ""
}
