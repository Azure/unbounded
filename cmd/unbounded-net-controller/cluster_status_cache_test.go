// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestNodeUpdateAffectsClusterStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	base := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "node-a",
			ResourceVersion: "1",
			Labels:          map[string]string{"site": "edge"},
			Annotations: map[string]string{
				unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation:          "203.0.113.10",
				unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation: now.Add(time.Hour).Format(time.RFC3339),
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "provider-a"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
		},
	}

	tests := []struct {
		name   string
		mutate func(*corev1.Node)
		want   bool
	}{
		{
			name: "resource version only",
			mutate: func(node *corev1.Node) {
				node.ResourceVersion = "2"
			},
		},
		{
			name: "fresh expiry extension",
			mutate: func(node *corev1.Node) {
				node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation] = now.Add(2 * time.Hour).Format(time.RFC3339)
			},
		},
		{
			name: "fresh discovery expires",
			mutate: func(node *corev1.Node) {
				node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation] = now.Add(-time.Second).Format(time.RFC3339)
			},
			want: true,
		},
		{
			name: "discovered address",
			mutate: func(node *corev1.Node) {
				node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] = "203.0.113.11"
			},
			want: true,
		},
		{
			name: "label",
			mutate: func(node *corev1.Node) {
				node.Labels["site"] = "other"
			},
			want: true,
		},
		{
			name: "status",
			mutate: func(node *corev1.Node) {
				node.Status.Addresses[0].Address = "10.0.0.11"
			},
			want: true,
		},
		{
			name: "other annotation",
			mutate: func(node *corev1.Node) {
				node.Annotations["example.com/value"] = "changed"
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := base.DeepCopy()
			tt.mutate(updated)

			if got := nodeUpdateAffectsClusterStatus(base, updated, now); got != tt.want {
				t.Fatalf("nodeUpdateAffectsClusterStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClusterStatusCacheNewReturnsNilStatus verifies that a newly created
// cache returns nil from Get before any Rebuild.
func TestClusterStatusCacheNewReturnsNilStatus(t *testing.T) {
	h := &healthState{clientset: k8sfake.NewClientset(), statusCache: NewNodeStatusCache()}

	c := NewClusterStatusCache(h)
	if got := c.Get(); got != nil {
		t.Fatalf("expected nil before Rebuild, got %+v", got)
	}

	if got := c.GetSeq(); got != 0 {
		t.Fatalf("expected seq=0 before Rebuild, got %d", got)
	}
}

// TestClusterStatusCacheRebuild verifies that Rebuild populates the cache.
func TestClusterStatusCacheRebuild(t *testing.T) {
	h := &healthState{clientset: k8sfake.NewClientset(), statusCache: NewNodeStatusCache()}
	c := NewClusterStatusCache(h)

	c.Rebuild(context.Background())

	got := c.Get()
	if got == nil {
		t.Fatal("expected non-nil status after Rebuild")
	}

	if got.Seq != 1 {
		t.Fatalf("expected seq=1 after first Rebuild, got %d", got.Seq)
	}
	// Second rebuild increments seq.
	c.Rebuild(context.Background())

	got = c.Get()
	if got.Seq != 2 {
		t.Fatalf("expected seq=2 after second Rebuild, got %d", got.Seq)
	}
}

func TestClusterStatusCachePatchNodeResolvesControllerOwnedExternalIPs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		node     *corev1.Node
		existing []string
		want     []string
	}{
		{
			name: "existing entry is refreshed from provider address",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{
					Type: corev1.NodeExternalIP, Address: "203.0.113.10",
				}}},
			},
			existing: []string{"192.0.2.10"},
			want:     []string{"203.0.113.10"},
		},
		{
			name:     "existing entry is cleared without a source",
			node:     &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
			existing: []string{"192.0.2.10"},
		},
		{
			name: "new entry uses discovered address",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Annotations: map[string]string{
					unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation:          "203.0.113.11",
					unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation: time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}},
			want: []string{"203.0.113.11"},
		},
		{
			name: "new entry is cleared when Kubernetes Node is missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if tt.node != nil {
				if err := indexer.Add(tt.node); err != nil {
					t.Fatalf("add Node: %v", err)
				}
			}

			c := NewClusterStatusCache(&healthState{nodeLister: corev1listers.NewNodeLister(indexer)})

			c.status = &ClusterStatusResponse{}
			if tt.existing != nil {
				c.status.Nodes = []*NodeStatusResponse{{NodeInfo: NodeInfo{Name: "node-a", ExternalIPs: tt.existing}}}
				c.nodeIndex["node-a"] = 0
			}

			incoming := NodeStatusResponse{NodeInfo: NodeInfo{
				Name:        "node-a",
				ExternalIPs: []string{"198.51.100.10"},
			}}
			c.PatchNode("node-a", incoming)

			if got := c.Get().Nodes[0].NodeInfo.ExternalIPs; !slices.Equal(got, tt.want) {
				t.Fatalf("ExternalIPs = %#v, want %#v", got, tt.want)
			}

			if !slices.Equal(incoming.NodeInfo.ExternalIPs, []string{"198.51.100.10"}) {
				t.Fatalf("PatchNode mutated incoming status: %#v", incoming.NodeInfo.ExternalIPs)
			}
		})
	}
}

// TestClusterStatusCacheMarkDirtyAndRun verifies that MarkFullRebuildNeeded triggers a
// rebuild within the Run loop.
func TestClusterStatusCacheMarkDirtyAndRun(t *testing.T) {
	h := &healthState{clientset: k8sfake.NewClientset(), statusCache: NewNodeStatusCache()}
	c := NewClusterStatusCache(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Run(ctx)

	// Run performs an initial Rebuild, wait for it.
	deadline := time.After(3 * time.Second)

	for c.Get() == nil {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial Rebuild in Run loop")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	initialSeq := c.GetSeq()

	// Mark dirty and wait for seq to advance (coalesce delay is 2s).
	c.MarkFullRebuildNeeded()

	deadline = time.After(5 * time.Second)

	for c.GetSeq() <= initialSeq {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for dirty rebuild; seq stuck at %d", c.GetSeq())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestClusterStatusCacheRunStopsOnCancel verifies the Run loop exits cleanly.
func TestClusterStatusCacheRunStopsOnCancel(t *testing.T) {
	h := &healthState{clientset: k8sfake.NewClientset(), statusCache: NewNodeStatusCache()}
	c := NewClusterStatusCache(h)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}
