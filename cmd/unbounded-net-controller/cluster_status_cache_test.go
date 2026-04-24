// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"
	"time"

	k8sfake "k8s.io/client-go/kubernetes/fake"
)

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
