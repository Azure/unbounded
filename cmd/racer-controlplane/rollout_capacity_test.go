// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model the reported rollout geometry with real reconciliation and durable
// history. Worker acknowledgments are synthetic; this does not model live worker
// retirement or prove that a particular cluster can reach the retirement barrier.
func TestForwardCapacityLargeUniversePodRollout(t *testing.T) {
	ctx := context.Background()

	const participants, retained = 1500, 126

	var (
		nodes   []corev1.Node
		pods    []corev1.Pod
		objects []client.Object
	)

	for i := range participants {
		n, p, _ := fixtures()
		n.Name = fmt.Sprintf("node-%04d", i)
		n.UID = types.UID(n.Name)
		p.Name, p.UID, p.Spec.NodeName = n.Name, types.UID("pod-"+n.Name), n.Name
		p.Status.PodIP = fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)

		nodes, pods = append(nodes, *n), append(pods, *p)
		objects = append(objects, n, p)
	}

	_, _, svc := fixtures()
	svc.Annotations[annotationPrefix+"slot-count"] = fmt.Sprint(participants)
	objects = append(objects, svc)
	kube := fakeKube(objects...)
	store := stateStore{client: kube, namespace: "state"}

	g, _, err := buildGeneration("default", nil, nodes, pods, []corev1.Service{*svc})
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 5
	if err := store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{controlStore: store}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	r, err := s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.persistPhase(ctx, "default", r, 4); err != nil {
		t.Fatal(err)
	}

	var history []forwardDecision

	for i := range retained {
		m := g.Nodes[nodes[i].Name]
		snap := index.snapshot(m.ID)
		snap.Revision, snap.Epoch = 4, 4

		data, err := marshalSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}

		history = append(history, forwardDecision{Snapshot: data, PodUID: m.PodUID})
	}

	if err := s.saveForwards(ctx, r, history); err != nil {
		t.Fatal(err)
	}

	before := r.pointer.Data["forwards"]
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	// Replace a recipient with retained history, then one without it. Each
	// controller restart must recollect actual process retirement acknowledgments.
	for iteration, replacement := range []int{0, participants - 1} {
		p := &corev1.Pod{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(&pods[replacement]), p); err != nil {
			t.Fatal(err)
		}

		if err := kube.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}

		p.ResourceVersion = ""

		p.UID = types.UID(fmt.Sprintf("replacement-%d", iteration))
		if err := kube.Create(ctx, p); err != nil {
			t.Fatal(err)
		}

		rec := newTestReconciler(kube)

		for _, phase := range []uint32{0, 3, 4} {
			if phase != 0 {
				roll := rec.server.rollouts["default"]
				for _, m := range g.Nodes {
					roll.acks[m.ID] = rolloutAck{phase: phase}
				}
			}

			_, reconcileErr := rec.Reconcile(ctx, request)

			durable, pointer, err := store.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			cm := &corev1.ConfigMap{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: "state", Name: pointer.Name + "-rollout"}, cm); err != nil {
				t.Fatal(err)
			}

			if phase != 4 {
				if reconcileErr == nil || !strings.Contains(reconcileErr.Error(), "forward history capacity exhausted") {
					t.Fatalf("phase %d: expected forward metadata backpressure, got %v", phase, reconcileErr)
				}

				if !reflect.DeepEqual(durable, g) || cm.Data["forwards"] != before {
					t.Fatal("failed admission changed durable topology or history")
				}

				continue
			}

			if reconcileErr != nil || durable.Revision != g.Revision+1 || durable.Nodes[p.Spec.NodeName].PodUID != string(p.UID) {
				t.Fatalf("retired survivors did not admit replacement: revision=%d error=%v", durable.Revision, reconcileErr)
			}

			if cm.Data["forwards"] != before {
				t.Fatal("planner collected replacement history before post-commit reload")
			}

			g = durable
		}

		// Reload from durable state, exercising only the existing post-commit GC.
		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		s = &Server{controlStore: store}
		if err := s.install(index); err != nil {
			t.Fatal(err)
		}

		r, err = s.rolloutFor(ctx, index)
		if err != nil {
			t.Fatal(err)
		}

		remaining, err := forwardHistory(r.pointer.Data["forwards"], "default", g.Revision)
		if err != nil || len(remaining) != retained-1 {
			t.Fatalf("unexpected post-commit history: count=%d error=%v", len(remaining), err)
		}

		for _, d := range remaining {
			if d.PodUID == string(pods[0].UID) || d.Boot != "" || d.Ref.Revision != 4 {
				t.Fatal("replacement GC lost or rebound another Pod's unknown-boot obligation")
			}
		}

		before = r.pointer.Data["forwards"]
		if err := s.persistPhase(ctx, "default", r, 4); err != nil {
			t.Fatal(err)
		}
	}
}
