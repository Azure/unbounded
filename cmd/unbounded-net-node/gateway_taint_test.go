// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const gatewayTaintTestNode = "gateway-a"

func TestRemoveGatewayNodeTaintPreservesOtherTuples(t *testing.T) {
	protected := []corev1.Taint{
		{Key: GatewayNodeTaintKey, Value: "true", Effect: corev1.TaintEffectNoExecute},
		{Key: GatewayNodeTaintKey, Value: "false", Effect: corev1.TaintEffectNoSchedule},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayTaintTestNode},
		Spec: corev1.NodeSpec{Taints: append(append([]corev1.Taint{}, protected...),
			corev1.Taint{Key: GatewayNodeTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule})},
	}

	client := fake.NewClientset(node)
	for range 2 {
		if err := removeGatewayNodeTaint(t.Context(), client, node.Name); err != nil {
			t.Fatal(err)
		}
	}

	got, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Spec.Taints) != len(protected) {
		t.Fatalf("taints=%v, want %v", got.Spec.Taints, protected)
	}

	for i, taint := range protected {
		if got.Spec.Taints[i] != taint {
			t.Fatalf("taints=%v, want %v", got.Spec.Taints, protected)
		}
	}

	if countPatches(client) != 1 {
		t.Fatalf("patches=%d, want only exact owned tuple removal", countPatches(client))
	}
}

func setGatewayTaintRemovalBackoff(t *testing.T, attempts int, initial, maxDelay time.Duration) {
	t.Helper()

	oldAttempts, oldInitial, oldMax := gatewayTaintRemovalMaxAttempts, gatewayTaintRemovalInitialDelay, gatewayTaintRemovalMaxDelay
	gatewayTaintRemovalMaxAttempts, gatewayTaintRemovalInitialDelay, gatewayTaintRemovalMaxDelay = attempts, initial, maxDelay

	t.Cleanup(func() {
		gatewayTaintRemovalMaxAttempts, gatewayTaintRemovalInitialDelay, gatewayTaintRemovalMaxDelay = oldAttempts, oldInitial, oldMax
	})
}

func newGatewayTaintedNodeClient() *fake.Clientset {
	return fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayTaintTestNode},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key:    GatewayNodeTaintKey,
			Value:  "true",
			Effect: corev1.TaintEffectNoSchedule,
		}}},
	})
}

// jsonPatchTestFailure mirrors the API server response for a failed JSON patch
// test operation: a 422 without object details.
func jsonPatchTestFailure() error {
	return apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", "testing value /spec/taints/0 failed: test failed", 0, false)
}

// admissionPolicyDenial mirrors a ValidatingAdmissionPolicy denial with the
// default Invalid reason: a 422 that names the rejected object.
func admissionPolicyDenial() error {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, gatewayTaintTestNode, errors.New("ValidatingAdmissionPolicy 'unbounded-net-node' with binding 'unbounded-net-node' denied request: denied"))
	err.ErrStatus.Reason = metav1.StatusReasonInvalid
	err.ErrStatus.Code = http.StatusUnprocessableEntity

	return err
}

func countPatches(client *fake.Clientset) int {
	count := 0

	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			count++
		}
	}

	return count
}

func gatewayTaintPresent(t *testing.T, client *fake.Clientset) bool {
	t.Helper()

	node, err := client.CoreV1().Nodes().Get(context.Background(), gatewayTaintTestNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	for _, taint := range node.Spec.Taints {
		if taint.Key == GatewayNodeTaintKey {
			return true
		}
	}

	return false
}

func TestIsRetryableGatewayTaintPatchError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "conflict", err: apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, gatewayTaintTestNode, errors.New("changed")), want: true},
		{name: "json patch test failure", err: jsonPatchTestFailure(), want: true},
		{name: "admission policy denial", err: admissionPolicyDenial(), want: false},
		{name: "object validation failure", err: apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, gatewayTaintTestNode, nil), want: false},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, gatewayTaintTestNode, errors.New("no")), want: false},
		{name: "non API error", err: errors.New("network"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableGatewayTaintPatchError(tt.err); got != tt.want {
				t.Fatalf("isRetryableGatewayTaintPatchError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveGatewayNodeTaintRetriesPatchRace(t *testing.T) {
	setGatewayTaintRemovalBackoff(t, 5, time.Millisecond, time.Millisecond)

	client := newGatewayTaintedNodeClient()
	failures := 2

	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failures > 0 {
			failures--

			return true, nil, jsonPatchTestFailure()
		}

		return false, nil, nil
	})

	if err := removeGatewayNodeTaint(context.Background(), client, gatewayTaintTestNode); err != nil {
		t.Fatalf("removeGatewayNodeTaint returned error: %v", err)
	}

	if got := countPatches(client); got != 3 {
		t.Fatalf("patch attempts = %d, want 3", got)
	}

	if gatewayTaintPresent(t, client) {
		t.Fatal("gateway taint retained after successful retry")
	}
}

func TestRemoveGatewayNodeTaintDoesNotRetryAdmissionDenial(t *testing.T) {
	setGatewayTaintRemovalBackoff(t, 5, time.Millisecond, time.Millisecond)

	client := newGatewayTaintedNodeClient()
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, admissionPolicyDenial()
	})

	err := removeGatewayNodeTaint(context.Background(), client, gatewayTaintTestNode)
	if err == nil || !strings.Contains(err.Error(), "denied request") {
		t.Fatalf("removeGatewayNodeTaint error = %v, want admission denial", err)
	}

	if got := countPatches(client); got != 1 {
		t.Fatalf("patch attempts = %d, want 1", got)
	}

	if !gatewayTaintPresent(t, client) {
		t.Fatal("gateway taint removed despite denial")
	}
}

func TestRemoveGatewayNodeTaintBoundsPersistentRaces(t *testing.T) {
	setGatewayTaintRemovalBackoff(t, 3, time.Millisecond, time.Millisecond)

	client := newGatewayTaintedNodeClient()
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, gatewayTaintTestNode, errors.New("changed"))
	})

	err := removeGatewayNodeTaint(context.Background(), client, gatewayTaintTestNode)
	if err == nil || !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("removeGatewayNodeTaint error = %v, want bounded conflict", err)
	}

	if got := countPatches(client); got != 3 {
		t.Fatalf("patch attempts = %d, want 3", got)
	}

	if !gatewayTaintPresent(t, client) {
		t.Fatal("gateway taint removed despite persistent conflict")
	}
}

func TestRemoveGatewayNodeTaintStopsBackoffOnContextCancel(t *testing.T) {
	setGatewayTaintRemovalBackoff(t, 5, time.Hour, time.Hour)

	ctx, cancel := context.WithCancelCause(context.Background())
	cleanupDone := errors.New("cleanup deadline")
	client := newGatewayTaintedNodeClient()

	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel(cleanupDone)

		return true, nil, jsonPatchTestFailure()
	})

	start := time.Now()

	err := removeGatewayNodeTaint(ctx, client, gatewayTaintTestNode)
	if !errors.Is(err, cleanupDone) || !apierrors.IsInvalid(err) {
		t.Fatalf("removeGatewayNodeTaint error = %v, want patch failure joined with cancellation cause", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("removeGatewayNodeTaint waited %v after cancellation", elapsed)
	}

	if got := countPatches(client); got != 1 {
		t.Fatalf("patch attempts = %d, want 1", got)
	}
}
