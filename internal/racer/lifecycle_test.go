// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestLifecycleGatesAndCancellation(t *testing.T) {
	r := initializedTopology(t)

	l := newLifecycle(r.Publications)
	if !l.NeedLeaderElection() || l.Ready(nil) == nil {
		t.Fatal("follower ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncCache := make(chan struct{})
	l.waitForCacheSync = func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case <-syncCache:
			return true
		}
	}
	started := make(chan error, 1)

	go func() { started <- l.Start(ctx) }()

	l.SetIssuerReady(true)
	l.SetServingReady(true)
	reconcileTopology(t, r, ctx)

	if l.Ready(nil) == nil {
		t.Fatal("ready before synchronized inputs")
	}

	close(syncCache)

	waitCtx, stopWait := context.WithTimeout(ctx, 5*time.Second)
	defer stopWait()

	if err := l.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}

	if err := l.Ready(nil); err != nil {
		t.Fatal(err)
	}

	l.SetIssuerReady(false)

	if l.Ready(nil) == nil {
		t.Fatal("ready without usable issuer")
	}

	l.SetIssuerReady(true)
	l.SetServingReady(false)

	if l.Ready(nil) == nil {
		t.Fatal("ready without listener")
	}

	if err := l.Wait(waitCtx); err != nil {
		t.Fatalf("listener gate requires listener: %v", err)
	}

	cancel()

	if err := <-started; err != nil {
		t.Fatal(err)
	}

	l.SetIssuerReady(true)
	l.SetServingReady(true)

	if l.Ready(nil) == nil {
		t.Fatal("old leadership resurrected")
	}

	if err := l.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait resurrected: %v", err)
	}

	if err := l.Start(context.Background()); !errors.Is(err, wire.Conflict) {
		t.Fatalf("leadership restarted: %v", err)
	}
}

func TestLifecycleWaitsForPublicationAndCancelsBeforeStartup(t *testing.T) {
	r := initializedTopology(t)
	l := newLifecycle(r.Publications)
	l.waitForCacheSync = func(context.Context) bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- l.Start(ctx) }()

	l.SetIssuerReady(true)

	waitCtx, stop := context.WithCancel(context.Background())
	stop()

	if err := l.Wait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait ignores request cancellation: %v", err)
	}

	ready := make(chan error, 1)

	go func() { ready <- l.Wait(ctx) }()

	select {
	case err := <-ready:
		t.Fatalf("ready without publication: %v", err)
	default:
	}

	reconcileTopology(t, r, ctx)

	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication notification lost")
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInitialEnqueueEmptyInputsAndCoalescing(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	for range 10 {
		if err := initialEnqueue().Start(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}

	if q.Len() != 1 {
		t.Fatalf("startup events not coalesced: %d", q.Len())
	}

	request, stopped := q.Get()
	if stopped || request != singleton(context.Background(), nil)[0] {
		t.Fatalf("initial request: %+v", request)
	}

	q.Done(request)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := initialEnqueue().Start(ctx, q); !errors.Is(err, context.Canceled) || q.Len() != 0 {
		t.Fatalf("canceled startup queued: %v", err)
	}
}

func TestTopologyWatchFiltering(t *testing.T) {
	cfg := testConfig(t)
	node := memberNode()
	updated := node.DeepCopy()

	updated.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("node readiness enqueued topology")
	}

	updated.Annotations = map[string]string{wire.SharesAnnotation: ""}
	if !nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("absent -> invalid empty annotation lost")
	}

	updated.Annotations = nil

	updated.Labels = map[string]string{wire.ExclusionLabel: "false"}
	if !nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("exclusion presence lost")
	}

	pod := memberPod("pod", 1, "192.0.2.1")
	pod.OwnerReferences[0].Name = cfg.DaemonSetName

	pred := managedPodChanges(cfg)
	if !pred.Create(event.CreateEvent{Object: &pod}) {
		t.Fatal("managed pod ignored")
	}

	changed := pod.DeepCopy()

	changed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if pred.Update(event.UpdateEvent{ObjectOld: &pod, ObjectNew: changed}) {
		t.Fatal("pod readiness enqueued topology")
	}

	changed.OwnerReferences = nil
	if pred.Create(event.CreateEvent{Object: changed}) || !pred.Update(event.UpdateEvent{ObjectOld: &pod, ObjectNew: changed}) {
		t.Fatal("ownership loss filtering")
	}

	changed = pod.DeepCopy()

	changed.Namespace = "unrelated"
	if pred.Create(event.CreateEvent{Object: changed}) {
		t.Fatal("foreign namespace pod admitted")
	}

	if keys := podNodeKeys(&pod); len(keys) != 1 || keys[0] != pod.Spec.NodeName {
		t.Fatalf("node index: %v", keys)
	}

	pod.Spec.NodeName = ""
	if len(podNodeKeys(&pod)) != 0 {
		t.Fatal("unassigned pod indexed")
	}

	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace, cm.ResourceVersion = cfg.VersionConfigMapName, cfg.Namespace, "1"
	newCM := cm.DeepCopy()

	newCM.ResourceVersion = "2"
	if versionChanges(cfg).Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: newCM}) {
		t.Fatal("CAS-only write caused reconcile loop")
	}

	newCM.Data = map[string]string{"sequence": "2"}
	if !versionChanges(cfg).Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: newCM}) {
		t.Fatal("version change ignored")
	}
}
