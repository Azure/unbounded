// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func startupWatchNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", UID: "node-uid", ResourceVersion: "10",
		Labels: map[string]string{
			unboundedv1alpha3.MachineSiteLabelKey:    "site-a",
			unboundednetv1alpha1.GatewayPoolLabelKey: "pool-a",
		},
	}}
}

func TestStartupAssignmentWatchDetectsChangesBeforeAcknowledgment(t *testing.T) {
	for _, change := range []string{"site", "pool", "demotion", "uid", "delete"} {
		t.Run(change, func(t *testing.T) {
			node := startupWatchNode()

			adopted, err := assignmentFromNode(node)
			if err != nil {
				t.Fatal(err)
			}

			client := fake.NewClientset(node)
			watcher := watch.NewRaceFreeFake()
			started := make(chan struct{})

			client.PrependWatchReactor("nodes", func(action k8stesting.Action) (bool, watch.Interface, error) {
				restrictions := action.(k8stesting.WatchAction).GetWatchRestrictions()
				if restrictions.Fields.String() != "metadata.name=node-a" || restrictions.ResourceVersion != "10" {
					t.Errorf("watch restrictions=%#v", restrictions)
				}

				close(started)

				return true, watcher, nil
			})
			ctx, monitor := startStartupAssignmentWatch(t.Context(), client, node.Name, adopted)
			t.Cleanup(monitor.stop)

			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("watch did not start")
			}
			// No successful topology reconciliation or acknowledgment occurs.
			changed := node.DeepCopy()

			switch change {
			case "site":
				changed.Labels[unboundedv1alpha3.MachineSiteLabelKey] = "site-b"
			case "pool":
				changed.Labels[unboundednetv1alpha1.GatewayPoolLabelKey] = "pool-b"
			case "demotion":
				delete(changed.Labels, unboundednetv1alpha1.GatewayPoolLabelKey)
			case "uid":
				changed.UID = "replacement"
			}

			if change == "delete" {
				watcher.Delete(changed)
			} else {
				watcher.Modify(changed)
			}

			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("assignment mismatch did not cancel failed startup")
			}

			if !errors.Is(context.Cause(ctx), errAssignmentChanged) {
				t.Fatalf("cause=%v", context.Cause(ctx))
			}

			monitor.stop()

			if !watcher.IsStopped() {
				t.Fatal("mismatch watch still running")
			}

			if err := acknowledgeEffectiveAssignment(ctx, client, "system", "agent", "pod-uid", node.Name, adopted); !errors.Is(err, errAssignmentChanged) {
				t.Fatalf("acknowledgment after mismatch=%v", err)
			}

			for _, action := range client.Actions() {
				if action.GetResource().Resource == "pods" {
					t.Fatalf("unexpected acknowledgment action: %v", action)
				}
			}
		})
	}
}

func TestStartupAssignmentWatchStopsAfterAcknowledgment(t *testing.T) {
	node := startupWatchNode()

	adopted, err := assignmentFromNode(node)
	if err != nil {
		t.Fatal(err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "system", UID: "pod-uid"},
		Spec:       corev1.PodSpec{NodeName: node.Name},
	}
	client := fake.NewClientset(node, pod)
	started := make(chan struct{})
	watcher := watch.NewRaceFreeFake()

	client.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
		close(started)
		return true, watcher, nil
	})
	ctx, monitor := startStartupAssignmentWatch(t.Context(), client, node.Name, adopted)
	t.Cleanup(monitor.stop)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("watch did not start")
	}

	patches := 0

	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 1 {
			return true, nil, errors.New("temporary acknowledgment failure")
		}

		return false, nil, nil
	})

	if err := acknowledgeEffectiveAssignment(ctx, client, pod.Namespace, pod.Name, pod.UID, node.Name, adopted); err == nil {
		t.Fatal("expected acknowledgment failure")
	}

	if watcher.IsStopped() || ctx.Err() != nil {
		t.Fatal("failed acknowledgment stopped assignment monitoring")
	}

	if err := acknowledgeEffectiveAssignment(ctx, client, pod.Namespace, pod.Name, pod.UID, node.Name, adopted); err != nil {
		t.Fatal(err)
	}

	monitor.stop()
	monitor.stop()

	if !watcher.IsStopped() {
		t.Fatal("acknowledged agent retained Node watch")
	}

	if ctx.Err() != nil {
		t.Fatalf("handoff canceled running agent: %v", context.Cause(ctx))
	}

	got, err := client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got.Annotations[unboundednetv1alpha1.EffectiveSiteAnnotation] != adopted.site ||
		got.Annotations[unboundednetv1alpha1.EffectiveGatewayPoolAnnotation] != adopted.gatewayPool {
		t.Fatalf("effective acknowledgment=%v", got.Annotations)
	}
}

func TestStartupAssignmentWatchRevalidatesAfterDisconnect(t *testing.T) {
	for _, disconnect := range []string{"closed", "expired"} {
		t.Run(disconnect, func(t *testing.T) {
			node := startupWatchNode()

			adopted, err := assignmentFromNode(node)
			if err != nil {
				t.Fatal(err)
			}

			client := fake.NewClientset(node)

			watcher := watch.NewRaceFreeFake()
			if disconnect == "closed" {
				watcher.Stop()
			} else {
				watcher.Error(&metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonExpired, Code: 410})
			}

			gets := 0

			client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				gets++

				current := node.DeepCopy()
				if gets > 1 {
					current.UID = "replacement"
				}

				return true, current, nil
			})
			client.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
				return true, watcher, nil
			})
			ctx, monitor := startStartupAssignmentWatch(t.Context(), client, node.Name, adopted)
			t.Cleanup(monitor.stop)

			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("disconnect did not revalidate assignment")
			}

			monitor.stop()

			if !errors.Is(context.Cause(ctx), errAssignmentChanged) {
				t.Fatalf("cause=%v", context.Cause(ctx))
			}

			if gets != 2 {
				t.Fatalf("GETs=%d, want initial and reconnect validation", gets)
			}
		})
	}
}

func TestStartupAssignmentWatchDetectsChangeBeforeWatchStarts(t *testing.T) {
	node := startupWatchNode()

	adopted, err := assignmentFromNode(node)
	if err != nil {
		t.Fatal(err)
	}

	node.UID = "replacement"
	client := fake.NewClientset(node)

	ctx, monitor := startStartupAssignmentWatch(t.Context(), client, node.Name, adopted)
	defer monitor.stop()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("initial mismatch was not detected")
	}

	if !errors.Is(context.Cause(ctx), errAssignmentChanged) {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "watch" {
			t.Fatal("watch started for replaced Node")
		}
	}
}
