// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestAssignmentFromNodeCanonicalAndGatewayValidation(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a",
		UID:  "node-uid",
		Labels: map[string]string{
			unboundedv1alpha3.MachineSiteLabelKey:         "canonical",
			unboundednetv1alpha1.SiteLabelKey:             "legacy",
			unboundednetv1alpha1.GatewayPoolLabelKey:      "pool.raw",
			"net.unbounded-cloud.io/unrelated-assignment": "ignored",
		},
	}}

	got, err := assignmentFromNode(node)
	if err != nil {
		t.Fatal(err)
	}

	if got.site != "canonical" || got.gatewayPool != "pool.raw" || got.nodeUID != "node-uid" {
		t.Fatalf("assignment = %#v", got)
	}

	node.Labels[unboundedv1alpha3.MachineSiteLabelKey] = ""

	got, err = assignmentFromNode(node)
	if err != nil || got.site != "legacy" {
		t.Fatalf("legacy fallback assignment = %#v, err=%v", got, err)
	}

	node.Labels[unboundednetv1alpha1.GatewayPoolLabelKey] = strings.Repeat("a", 64)
	if _, err := assignmentFromNode(node); err == nil {
		t.Fatal("expected invalid raw GatewayPool label value to fail")
	}
}

func TestWaitForNodeAssignmentWatchRestartAndCancel(t *testing.T) {
	t.Run("assignment arrives through watch", func(t *testing.T) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "uid-a", ResourceVersion: "1"}}
		client := fake.NewClientset(node)
		watcher := watch.NewFake()
		watchStarted := make(chan struct{})

		client.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
			close(watchStarted)

			return true, watcher, nil
		})

		go func() {
			<-watchStarted

			assigned := node.DeepCopy()
			assigned.Labels = map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-a"}
			assigned.ResourceVersion = "2"
			watcher.Modify(assigned)
		}()

		got, err := waitForNodeAssignment(t.Context(), client, node.Name)
		if err != nil {
			t.Fatal(err)
		}

		if got.site != "site-a" || got.nodeUID != node.UID {
			t.Fatalf("assignment = %#v", got)
		}
	})

	t.Run("closed watch restarts from get", func(t *testing.T) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "uid-a", ResourceVersion: "1"}}
		client := fake.NewClientset(node)
		closed := watch.NewFake()
		closed.Stop()

		watchCalls := 0
		getCalls := 0

		client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			getCalls++

			current := node.DeepCopy()
			if getCalls > 1 {
				current.Labels = map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-a"}
				current.ResourceVersion = "2"
			}

			return true, current, nil
		})

		client.PrependWatchReactor("nodes", func(action k8stesting.Action) (bool, watch.Interface, error) {
			watchCalls++
			return true, closed, nil
		})

		got, err := waitForNodeAssignment(t.Context(), client, node.Name)
		if err != nil {
			t.Fatal(err)
		}

		if got.site != "site-a" || watchCalls != 1 {
			t.Fatalf("assignment=%#v watchCalls=%d", got, watchCalls)
		}
	})

	t.Run("cancel active watch", func(t *testing.T) {
		client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
		watcher := watch.NewFake()

		client.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, watcher, nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := waitForNodeAssignment(ctx, client, "node-a"); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})
}

func TestAcknowledgeEffectiveAssignment(t *testing.T) {
	adopted := nodeAssignment{site: "site-a", gatewayPool: "", nodeUID: "node-uid"}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		UID:    adopted.nodeUID,
		Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: adopted.site},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: "agent", UID: "pod-uid"},
		Spec:       corev1.PodSpec{NodeName: node.Name},
	}
	client := fake.NewClientset(node, pod)

	if err := acknowledgeEffectiveAssignment(t.Context(), client, pod.Namespace, pod.Name, pod.UID, node.Name, adopted); err != nil {
		t.Fatal(err)
	}

	got, err := client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got.Annotations[unboundednetv1alpha1.EffectiveSiteAnnotation] != "site-a" {
		t.Fatalf("effective Site annotations = %#v", got.Annotations)
	}

	if value, ok := got.Annotations[unboundednetv1alpha1.EffectiveGatewayPoolAnnotation]; !ok || value != "" {
		t.Fatalf("effective GatewayPool annotation = %q, present=%v", value, ok)
	}

	delete(got.Annotations, unboundednetv1alpha1.EffectiveGatewayPoolAnnotation)

	if _, err := client.CoreV1().Pods(pod.Namespace).Update(t.Context(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := acknowledgeEffectiveAssignment(t.Context(), client, pod.Namespace, pod.Name, pod.UID, node.Name, adopted); err != nil {
		t.Fatal(err)
	}

	got, err = client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if value, ok := got.Annotations[unboundednetv1alpha1.EffectiveGatewayPoolAnnotation]; !ok || value != "" {
		t.Fatalf("missing worker acknowledgment was not restored: value=%q present=%v", value, ok)
	}

	if err := acknowledgeEffectiveAssignment(t.Context(), client, pod.Namespace, pod.Name, types.UID("replacement"), node.Name, adopted); err == nil {
		t.Fatal("expected Pod UID mismatch")
	}

	current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	current.Labels[unboundedv1alpha3.MachineSiteLabelKey] = "site-b"
	if _, err := client.CoreV1().Nodes().Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	err = acknowledgeEffectiveAssignment(t.Context(), client, pod.Namespace, pod.Name, pod.UID, node.Name, adopted)
	if !errors.Is(err, errAssignmentChanged) {
		t.Fatalf("error = %v, want assignment changed", err)
	}
}

func TestWaitForAssignmentRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	if err := waitForAssignmentRetry(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestAssignmentTransitionRequired(t *testing.T) {
	base := nodeAssignment{site: "site-a", gatewayPool: "pool-a", nodeUID: "uid-a"}
	tests := []struct {
		name    string
		current nodeAssignment
		want    bool
	}{
		{name: "unchanged", current: base},
		{name: "site changed", current: nodeAssignment{site: "site-b", gatewayPool: "pool-a", nodeUID: "uid-a"}, want: true},
		{name: "site unset", current: nodeAssignment{gatewayPool: "pool-a", nodeUID: "uid-a"}, want: true},
		{name: "gateway changed", current: nodeAssignment{site: "site-a", gatewayPool: "pool-b", nodeUID: "uid-a"}, want: true},
		{name: "gateway unset", current: nodeAssignment{site: "site-a", nodeUID: "uid-a"}, want: true},
		{name: "node replaced", current: nodeAssignment{site: "site-a", gatewayPool: "pool-a", nodeUID: "uid-b"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assignmentTransitionRequired(base, tt.current); got != tt.want {
				t.Fatalf("assignmentTransitionRequired() = %v, want %v", got, tt.want)
			}
		})
	}
}
