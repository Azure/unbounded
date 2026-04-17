// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"regexp"
	"testing"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/Azure/unbounded-kube/internal/net/allocator"
)

// TestControllerInformerSynced tests controller informer synced.
func TestControllerInformerSynced(t *testing.T) {
	c := &Controller{nodeSynced: func() bool { return true }}
	if !c.InformerSynced() {
		t.Fatalf("expected informer synced to be true")
	}

	c.nodeSynced = func() bool { return false }
	if c.InformerSynced() {
		t.Fatalf("expected informer synced to be false")
	}
}

// TestControllerMatchesNodeFilter tests controller matches node filter.
func TestControllerMatchesNodeFilter(t *testing.T) {
	c := &Controller{}
	if !c.matchesNodeFilter("any-node") {
		t.Fatalf("expected match when no regex configured")
	}

	c.nodeRegex = regexp.MustCompile(`^node-[0-9]+$`)
	if !c.matchesNodeFilter("node-12") {
		t.Fatalf("expected regex-matching node to pass filter")
	}

	if c.matchesNodeFilter("worker-a") {
		t.Fatalf("expected non-matching node to fail filter")
	}
}

// TestControllerEnqueueNode tests controller enqueue node.
func TestControllerEnqueueNode(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "test"})
	defer q.ShutDown()

	c := &Controller{workqueue: q}
	c.enqueueNode(cache.ExplicitKey("node-a"))

	if got := q.Len(); got != 1 {
		t.Fatalf("expected queue length 1 after enqueue, got %d", got)
	}

	obj, shutdown := q.Get()
	if shutdown {
		t.Fatalf("expected queue item, got shutdown")
	}

	q.Done(obj)

	if obj != "node-a" {
		t.Fatalf("unexpected queued key: %q", obj)
	}
}

// TestControllerProcessNextWorkItemNotFoundNode verifies that processNextWorkItem
// gracefully handles a node key not found in the lister (the node was deleted).
func TestControllerProcessNextWorkItemNotFoundNode(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "test"})
	defer q.ShutDown()

	client := fake.NewClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	nodeInformer := factory.Core().V1().Nodes()

	alloc, err := allocator.NewAllocator(nil, nil, 24, 64)
	if err != nil {
		t.Fatalf("failed to create allocator: %v", err)
	}

	c := &Controller{
		workqueue:       q,
		nodeLister:      nodeInformer.Lister(),
		nodeSynced:      nodeInformer.Informer().HasSynced,
		allocator:       alloc,
		pendingReleases: make(map[string][]string),
	}

	q.Add("nonexistent-node")

	if ok := c.processNextWorkItem(context.Background()); !ok {
		t.Fatalf("expected worker loop to continue after not-found node")
	}

	if q.Len() != 0 {
		t.Fatalf("expected queue to be drained")
	}
}

// TestNewControllerRejectsInvalidRegex tests new controller rejects invalid regex.
func TestNewControllerRejectsInvalidRegex(t *testing.T) {
	client := fake.NewClientset()
	factory := informers.NewSharedInformerFactory(client, 0)

	alloc, err := allocator.NewAllocator(nil, nil, 24, 64)
	if err != nil {
		t.Fatalf("failed to create allocator: %v", err)
	}

	if _, err := NewController(client, factory, alloc, "[", nil); err == nil {
		t.Fatalf("expected invalid regex error")
	}
}
