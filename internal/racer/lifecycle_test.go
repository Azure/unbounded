// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

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
