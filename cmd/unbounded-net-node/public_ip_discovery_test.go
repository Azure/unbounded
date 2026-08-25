// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

type fakePublicIPDiscoverer struct {
	publicIP netip.Addr
	err      error
	calls    int
	server   string
}

type flakyPublicIPDiscoverer struct {
	failures int
	calls    int
}

type blockingPublicIPDiscoverer struct {
	calls int
}

type signalingPublicIPDiscoverer struct {
	calls chan struct{}
}

func (s signalingPublicIPDiscoverer) DiscoverPublicIP(ctx context.Context, _ string) (netip.Addr, error) {
	select {
	case s.calls <- struct{}{}:
		return netip.MustParseAddr("203.0.113.20"), nil
	case <-ctx.Done():
		return netip.Addr{}, ctx.Err()
	}
}

func testPublicIPDiscoveryConfig(enabled bool) publicIPDiscoveryConfig {
	return publicIPDiscoveryConfig{
		Enabled:              enabled,
		Server:               "stun.example.com:3478",
		RecheckInterval:      time.Hour,
		InitialDelayLimit:    time.Minute,
		CleanupRetryInterval: publicIPDiscoveryCleanupRetryInterval,
	}
}

func (f *fakePublicIPDiscoverer) DiscoverPublicIP(_ context.Context, server string) (netip.Addr, error) {
	f.calls++
	f.server = server

	return f.publicIP, f.err
}

func (f *flakyPublicIPDiscoverer) DiscoverPublicIP(_ context.Context, _ string) (netip.Addr, error) {
	f.calls++
	if f.calls <= f.failures {
		return netip.Addr{}, errors.New("STUN request failed")
	}

	return netip.MustParseAddr("203.0.113.20"), nil
}

func (b *blockingPublicIPDiscoverer) DiscoverPublicIP(ctx context.Context, _ string) (netip.Addr, error) {
	b.calls++

	<-ctx.Done()

	return netip.Addr{}, ctx.Err()
}

func TestRunPublicIPDiscoveryRechecks(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	cfg := testPublicIPDiscoveryConfig(true)
	cfg.InitialDelayLimit = 0
	cfg.RecheckInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	calls := make(chan struct{})

	go func() {
		runPublicIPDiscovery(ctx, clientset, "node-a", cfg, signalingPublicIPDiscoverer{calls: calls})
		close(done)
	}()

	for range 2 {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for public IP discovery")
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("public IP discovery did not stop after cancellation")
	}
}

func TestRunPublicIPDiscoveryRetriesDisabledCleanup(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Annotations: map[string]string{
		unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation: "203.0.113.20",
	}}}
	clientset := fake.NewClientset(node)
	getCalls := 0

	clientset.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, errors.New("transient get failure")
		}

		return false, nil, nil
	})

	cfg := testPublicIPDiscoveryConfig(false)
	cfg.InitialDelayLimit = 24 * time.Hour
	cfg.CleanupRetryInterval = time.Millisecond

	if delay := publicIPDiscoveryInitialDelay(node.Name, cfg.InitialDelayLimit); delay <= time.Second {
		t.Fatalf("test setup produced initial delay %s, want greater than one second", delay)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	runPublicIPDiscovery(ctx, clientset, node.Name, cfg, &fakePublicIPDiscoverer{})

	if getCalls != 2 {
		t.Fatalf("Node GET calls = %d, want 2", getCalls)
	}

	got, err := clientset.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Node after cleanup retry: %v", err)
	}

	if _, exists := got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation]; exists {
		t.Fatal("disabled cleanup retry left the discovered public IP annotation")
	}
}

func TestDiscoverAndAnnotateNodePublicIPRetriesDiscovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		failures    int
		wantErr     bool
		wantPatches int
	}{
		{name: "second attempt succeeds", failures: 1, wantPatches: 1},
		{name: "third attempt succeeds", failures: 2, wantPatches: 1},
		{name: "all attempts fail", failures: 3, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientset := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
			discoverer := &flakyPublicIPDiscoverer{failures: tt.failures}

			err := discoverAndAnnotateNodePublicIP(
				t.Context(),
				clientset,
				"node-a",
				testPublicIPDiscoveryConfig(true),
				discoverer,
				0,
				0,
			)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "failed after 3 attempts") {
					t.Fatalf("error = %v, want three-attempt failure", err)
				}
			} else if err != nil {
				t.Fatalf("discoverAndAnnotateNodePublicIP() error = %v", err)
			}

			wantCalls := min(tt.failures+1, 3)
			if discoverer.calls != wantCalls {
				t.Fatalf("DiscoverPublicIP() calls = %d, want %d", discoverer.calls, wantCalls)
			}

			actions := clientset.Actions()
			if len(actions) != 1+tt.wantPatches || actions[0].GetVerb() != "get" {
				t.Fatalf("client actions = %#v, want one GET and %d PATCH actions", actions, tt.wantPatches)
			}

			if tt.wantPatches == 1 && actions[1].GetVerb() != "patch" {
				t.Fatalf("second client action = %s, want patch", actions[1].GetVerb())
			}
		})
	}
}

func TestDiscoverPublicIPWithRetryStopsOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	discoverer := &blockingPublicIPDiscoverer{}

	_, err := discoverPublicIPWithRetry(ctx, discoverer, "stun.example.com:3478", 0, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}

	if discoverer.calls != 1 {
		t.Fatalf("DiscoverPublicIP() calls = %d, want 1", discoverer.calls)
	}
}

func TestPublicIPDiscoveryInitialDelayIsStableAndBounded(t *testing.T) {
	t.Parallel()

	const limit = 10 * time.Minute

	first := publicIPDiscoveryInitialDelay("node-a", limit)

	second := publicIPDiscoveryInitialDelay("node-a", limit)
	if first != second || first < 0 || first >= limit {
		t.Fatalf("delays = %s, %s; want equal values in [0, %s)", first, second, limit)
	}
}

func TestDiscoverAndAnnotateNodePublicIPCleansUpSupersededDiscovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		enabled       bool
		hasDeclaredIP bool
		declaredIP    string
		externalIP    bool
	}{
		{name: "disabled"},
		{name: "administrator declaration", enabled: true, hasDeclaredIP: true, declaredIP: "203.0.113.30"},
		{name: "empty administrator declaration", enabled: true, hasDeclaredIP: true},
		{name: "provider external IP", enabled: true, externalIP: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Annotations: map[string]string{
					unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation:          "203.0.113.20",
					unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation: time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}}
			if tt.hasDeclaredIP {
				node.Annotations[unboundednetv1alpha1.NodeDeclaredPublicIPAnnotation] = tt.declaredIP
			}

			if tt.externalIP {
				node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.40"}}
			}

			clientset := fake.NewClientset(node)
			discoverer := &fakePublicIPDiscoverer{publicIP: netip.MustParseAddr("203.0.113.50")}
			cfg := testPublicIPDiscoveryConfig(tt.enabled)

			if err := discoverAndAnnotateNodePublicIP(t.Context(), clientset, node.Name, cfg, discoverer); err != nil {
				t.Fatalf("discoverAndAnnotateNodePublicIP() error = %v", err)
			}

			got, err := clientset.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get Node: %v", err)
			}

			if _, exists := got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation]; exists {
				t.Fatal("discovered public IP was not removed")
			}

			if _, exists := got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation]; exists {
				t.Fatal("discovered public IP expiry was not removed")
			}

			clientset.ClearActions()

			if err := discoverAndAnnotateNodePublicIP(t.Context(), clientset, node.Name, cfg, discoverer); err != nil {
				t.Fatalf("second discoverAndAnnotateNodePublicIP() error = %v", err)
			}

			for _, action := range clientset.Actions() {
				if action.GetVerb() == "patch" || action.GetVerb() == "update" {
					t.Fatalf("second cleanup emitted a write: %#v", action)
				}
			}

			if discoverer.calls != 0 {
				t.Fatalf("DiscoverPublicIP() calls = %d, want 0", discoverer.calls)
			}
		})
	}
}

func TestDiscoverAndAnnotateNodePublicIPRefreshesSuccessfulDiscovery(t *testing.T) {
	t.Parallel()

	const nodeName = "node-a"

	cfg := testPublicIPDiscoveryConfig(true)
	beforeDiscovery := time.Now().UTC()
	clientset := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: nodeName,
		Annotations: map[string]string{
			unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation:          "203.0.113.20",
			unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation: beforeDiscovery.Add(2 * cfg.RecheckInterval).Format(time.RFC3339Nano),
		},
	}})
	discoverer := &fakePublicIPDiscoverer{publicIP: netip.MustParseAddr("203.0.113.20")}

	if err := discoverAndAnnotateNodePublicIP(t.Context(), clientset, nodeName, cfg, discoverer); err != nil {
		t.Fatalf("discoverAndAnnotateNodePublicIP() error = %v", err)
	}

	afterDiscovery := time.Now().UTC()

	got, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Node: %v", err)
	}

	if got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] != "203.0.113.20" {
		t.Fatalf("discovered public IP = %q", got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation])
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation])
	if err != nil || expiresAt.Before(beforeDiscovery.Add(3*cfg.RecheckInterval)) || expiresAt.After(afterDiscovery.Add(3*cfg.RecheckInterval)) {
		t.Fatalf("expiry = %q, err = %v", got.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation], err)
	}

	if discoverer.calls != 1 || discoverer.server != cfg.Server {
		t.Fatalf("DiscoverPublicIP() calls = %d server = %q", discoverer.calls, discoverer.server)
	}
}

func TestDiscoverAndAnnotateNodePublicIPReturnsPatchError(t *testing.T) {
	t.Parallel()

	cfg := testPublicIPDiscoveryConfig(true)
	clientset := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	clientset.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("patch rejected")
	})

	err := discoverAndAnnotateNodePublicIP(t.Context(), clientset, "node-a", cfg, &fakePublicIPDiscoverer{publicIP: netip.MustParseAddr("203.0.113.20")})
	if err == nil || !strings.Contains(err.Error(), "patch rejected") {
		t.Fatalf("error = %v, want Node PATCH error", err)
	}
}
