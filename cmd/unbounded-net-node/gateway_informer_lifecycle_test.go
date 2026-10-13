// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestGatewayInformerLifecycleLateEventHandler(t *testing.T) {
	client := newGatewayLifecycleClient()
	lifecycle := newGatewayInformerLifecycle(client, 0)
	t.Cleanup(lifecycle.close)

	if _, _, err := lifecycle.prepare(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	// Start the generation before installing the callback, reproducing the
	// startup ordering that previously captured a permanent no-op.
	events := make(chan struct{}, 1)

	lifecycle.setEventHandler(func() {
		_ = lifecycle.snapshot()

		events <- struct{}{}
	})

	waitForEvent := func(resource, operation string) {
		t.Helper()

		select {
		case <-events:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s %s did not request an immediate reconcile", resource, operation)
		}
	}
	for _, resource := range []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{gatewayPoolGVR, "GatewayPool"},
		{gatewayNodeGVR, "GatewayPoolNode"},
		{gatewayPoolPeeringGVR, "GatewayPoolPeering"},
	} {
		api := client.Resource(resource.gvr)
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "net.unbounded-cloud.io/v1alpha1",
			"kind":       resource.kind,
			"metadata":   map[string]any{"name": "event-test", "resourceVersion": "1"},
			"spec":       map[string]any{"nodeName": "node-a"},
		}}

		created, err := api.Create(t.Context(), object, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		waitForEvent(resource.kind, "add")
		created.SetResourceVersion("2")

		if err := unstructured.SetNestedField(created.Object, "node-b", "spec", "nodeName"); err != nil {
			t.Fatal(err)
		}

		if _, err := api.Update(t.Context(), created, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}

		waitForEvent(resource.kind, "update")

		if err := api.Delete(t.Context(), created.GetName(), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}

		waitForEvent(resource.kind, "delete")
	}

	if got := watchActionCount(client); got != 3 {
		t.Fatalf("watch actions=%d, want unchanged generation with three watches", got)
	}
}

func TestGatewayCachesRequired(t *testing.T) {
	assignments := newTestInformer()
	adopted := nodeAssignment{site: "site-a"}

	if gatewayCachesRequired(adopted, adopted.site, assignments) {
		t.Fatal("empty assignment cache unexpectedly requires gateway caches")
	}

	disabled := false
	for _, assignment := range []*unboundednetv1alpha1.SiteGatewayPoolAssignment{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "remote"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Sites:        []string{"site-b"},
				GatewayPools: []string{"pool-b"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "disabled"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Enabled:      &disabled,
				Sites:        []string{"site-a"},
				GatewayPools: []string{"pool-disabled"},
			},
		},
	} {
		if err := assignments.GetStore().Add(toUnstructuredSiteWatch(t, assignment)); err != nil {
			t.Fatalf("add assignment: %v", err)
		}
	}

	if gatewayCachesRequired(adopted, adopted.site, assignments) {
		t.Fatal("remote and disabled assignments unexpectedly require gateway caches")
	}

	direct := &unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "direct"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites:        []string{"site-a"},
			GatewayPools: []string{"pool-a"},
		},
	}
	if err := assignments.GetStore().Add(toUnstructuredSiteWatch(t, direct)); err != nil {
		t.Fatalf("add direct assignment: %v", err)
	}

	if !gatewayCachesRequired(adopted, adopted.site, assignments) {
		t.Fatal("direct assignment did not require gateway caches")
	}

	if !gatewayCachesRequired(nodeAssignment{site: "site-a", gatewayPool: "pool-own"}, "site-a", newTestInformer()) {
		t.Fatal("pinned gateway role must always require gateway caches")
	}
}

func TestGatewayInformerLifecycle(t *testing.T) {
	client := newGatewayLifecycleClient()
	lifecycle := newGatewayInformerLifecycle(client, 0)
	t.Cleanup(lifecycle.close)

	if ready, reason := lifecycle.readiness(); !ready || reason != "" {
		t.Fatalf("intentionally absent caches readiness = %v, %q", ready, reason)
	}

	lifecycle.setRequired(true)

	if ready, reason := lifecycle.readiness(); ready || reason != "required gateway informer caches are not running" {
		t.Fatalf("missing required caches readiness = %v, %q", ready, reason)
	}

	var events atomic.Int32

	lifecycle.setEventHandler(func() {
		events.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshot, retire, err := lifecycle.prepare(ctx, true)
	if err != nil {
		t.Fatalf("prepare required caches: %v", err)
	}

	if retire || !gatewayInformerSnapshotSynced(snapshot) {
		t.Fatalf("required snapshot = %#v, retire=%v", snapshot, retire)
	}

	if ready, reason := lifecycle.readiness(); !ready || reason != "" {
		t.Fatalf("synced required caches readiness = %v, %q", ready, reason)
	}

	if got := watchActionCount(client); got != 3 {
		t.Fatalf("watch actions = %d, want one for each gateway resource", got)
	}

	retiring, retire, err := lifecycle.prepare(ctx, false)
	if err != nil {
		t.Fatalf("prepare cache retirement: %v", err)
	}

	if !retire || retiring.generation != snapshot.generation {
		t.Fatalf("retiring snapshot generation=%d retire=%v, want generation=%d", retiring.generation, retire, snapshot.generation)
	}

	// Readdition before cleanup keeps the synchronized generation alive.
	lifecycle.setRequired(true)
	lifecycle.retire(retiring.generation)

	if got := lifecycle.snapshot(); !got.available || got.generation != snapshot.generation {
		t.Fatalf("readdition lost synchronized generation: %#v", got)
	}

	lifecycle.setRequired(false)
	lifecycle.retire(snapshot.generation)

	if got := lifecycle.snapshot(); got.available {
		t.Fatalf("retired snapshot still available: %#v", got)
	}

	if ready, reason := lifecycle.readiness(); !ready || reason != "" {
		t.Fatalf("retired intentionally absent readiness = %v, %q", ready, reason)
	}
}

func TestGatewayInformerLifecycleConcurrentReadiness(t *testing.T) {
	lifecycle := newGatewayInformerLifecycle(newGatewayLifecycleClient(), 0)
	t.Cleanup(lifecycle.close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 100 {
				_, _ = lifecycle.readiness()
				_ = lifecycle.snapshot()
			}
		}()
	}

	if _, _, err := lifecycle.prepare(ctx, true); err != nil {
		t.Fatalf("prepare required caches: %v", err)
	}

	lifecycle.setRequired(false)
	lifecycle.retire(lifecycle.snapshot().generation)
	wg.Wait()
}

func newGatewayLifecycleClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gatewayPoolGVR:        "GatewayPoolList",
			gatewayNodeGVR:        "GatewayPoolNodeList",
			gatewayPoolPeeringGVR: "GatewayPoolPeeringList",
		},
	)
}

func watchActionCount(client *dynamicfake.FakeDynamicClient) int {
	count := 0

	for _, action := range client.Actions() {
		if action.GetVerb() == "watch" {
			count++
		}
	}

	return count
}
