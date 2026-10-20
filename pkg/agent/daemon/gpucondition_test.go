// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const gpuTestNode = "gpusrv132"

var kubeletReady = corev1.NodeCondition{
	Type:   corev1.NodeReady,
	Status: corev1.ConditionTrue,
	Reason: "KubeletReady",
}

// gpuConditionHarness reconciles one Node against GPU records written to a
// temporary machines directory, counting the status patches it makes.
type gpuConditionHarness struct {
	machinesDir string
	client      client.Client
	reconciler  *gpuConditionReconciler
	patches     int
}

func newGPUConditionHarness(t *testing.T, conditions ...corev1.NodeCondition) *gpuConditionHarness {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	h := &gpuConditionHarness{machinesDir: t.TempDir()}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: gpuTestNode},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}

	h.client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&corev1.Node{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				h.patches++

				require.Equal(t, types.StrategicMergePatchType, patch.Type(),
					"a merge patch would replace the conditions kubelet maintains")

				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	h.reconciler = &gpuConditionReconciler{client: h.client, machinesDir: h.machinesDir}

	return h
}

func (h *gpuConditionHarness) record(t *testing.T, machine string, status goalstates.GPUStatus) {
	t.Helper()

	require.NoError(t, goalstates.WriteGPUStatus(filepath.Join(h.machinesDir, machine), status))
}

func (h *gpuConditionHarness) reconcile(t *testing.T) []corev1.NodeCondition {
	t.Helper()

	_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: gpuTestNode}})
	require.NoError(t, err)

	var node corev1.Node
	require.NoError(t, h.client.Get(context.Background(), client.ObjectKey{Name: gpuTestNode}, &node))

	return node.Status.Conditions
}

func findNodeCondition(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}

	return nil
}

func TestGPUConditionReportsSetupFailure(t *testing.T) {
	t.Parallel()

	h := newGPUConditionHarness(t, kubeletReady)
	checked := time.Date(2026, 10, 20, 15, 37, 28, 500, time.UTC)
	h.record(t, goalstates.NSpawnMachineKube1, goalstates.GPUStatus{
		Reason:  goalstates.GPUReasonSetupFailed,
		Message: "setup-nvidia: nvidia-ctk cdi generate in kube1: exit status 1",
		Time:    checked,
	})

	conditions := h.reconcile(t)

	gpu := findNodeCondition(conditions, NodeConditionGPUReady)
	require.NotNil(t, gpu)
	require.Equal(t, corev1.ConditionFalse, gpu.Status)
	require.Equal(t, goalstates.GPUReasonSetupFailed, gpu.Reason)
	require.Equal(t, "setup-nvidia: nvidia-ctk cdi generate in kube1: exit status 1", gpu.Message)
	require.True(t, gpu.LastTransitionTime.Time.Equal(checked.Truncate(time.Second)))

	ready := findNodeCondition(conditions, corev1.NodeReady)
	require.NotNil(t, ready, "kubelet's conditions must survive")
	require.Equal(t, "KubeletReady", ready.Reason)
}

func TestGPUConditionReportsReady(t *testing.T) {
	t.Parallel()

	h := newGPUConditionHarness(t, kubeletReady)
	h.record(t, goalstates.NSpawnMachineKube2, goalstates.GPUStatus{
		Ready:   true,
		Reason:  goalstates.GPUReasonSetupSucceeded,
		Message: "NVIDIA GPU setup succeeded",
		Time:    time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
	})

	gpu := findNodeCondition(h.reconcile(t), NodeConditionGPUReady)
	require.NotNil(t, gpu)
	require.Equal(t, corev1.ConditionTrue, gpu.Status)
	require.Equal(t, goalstates.GPUReasonSetupSucceeded, gpu.Reason)
}

func TestGPUConditionDoesNotPatchUnchangedNode(t *testing.T) {
	t.Parallel()

	h := newGPUConditionHarness(t, kubeletReady)
	h.record(t, goalstates.NSpawnMachineKube1, goalstates.GPUStatus{
		Reason: goalstates.GPUReasonSetupFailed,
		// Sub-second precision is lost on the server; it must not read as a
		// change on the next reconcile.
		Time: time.Date(2026, 10, 20, 15, 37, 28, 123456789, time.UTC),
	})

	h.reconcile(t)
	h.reconcile(t)

	require.Equal(t, 1, h.patches, "an unchanged condition must not be patched again")
}

func TestGPUConditionFollowsLatestMachineStart(t *testing.T) {
	t.Parallel()

	h := newGPUConditionHarness(t)
	failed := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	h.record(t, goalstates.NSpawnMachineKube1, goalstates.GPUStatus{Reason: goalstates.GPUReasonSetupFailed, Time: failed})

	first := findNodeCondition(h.reconcile(t), NodeConditionGPUReady)
	require.NotNil(t, first)
	require.Equal(t, corev1.ConditionFalse, first.Status)

	// The GPUs work after the next start.
	fixed := failed.Add(time.Hour)
	h.record(t, goalstates.NSpawnMachineKube1, goalstates.GPUStatus{
		Ready:  true,
		Reason: goalstates.GPUReasonSetupSucceeded,
		Time:   fixed,
	})

	second := findNodeCondition(h.reconcile(t), NodeConditionGPUReady)
	require.NotNil(t, second)
	require.Equal(t, corev1.ConditionTrue, second.Status)
	require.True(t, second.LastTransitionTime.Time.Equal(fixed))
}

func TestGPUConditionRemovedWithoutRecord(t *testing.T) {
	t.Parallel()

	stale := corev1.NodeCondition{Type: NodeConditionGPUReady, Status: corev1.ConditionFalse, Reason: goalstates.GPUReasonSetupFailed}
	h := newGPUConditionHarness(t, kubeletReady, stale)

	conditions := h.reconcile(t)
	require.Nil(t, findNodeCondition(conditions, NodeConditionGPUReady))
	require.NotNil(t, findNodeCondition(conditions, corev1.NodeReady))

	h.reconcile(t)
	require.Equal(t, 1, h.patches, "a node without GPUs needs no further patches")
}

func TestGPUConditionIgnoresMissingNode(t *testing.T) {
	t.Parallel()

	h := newGPUConditionHarness(t)

	_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "other-node"}})
	require.NoError(t, err)
	require.Zero(t, h.patches)
}

func TestSetGPUReadyConditionKeepsTransitionTime(t *testing.T) {
	t.Parallel()

	transitioned := metav1.NewTime(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	conditions := []corev1.NodeCondition{{
		Type:               NodeConditionGPUReady,
		Status:             corev1.ConditionFalse,
		Reason:             goalstates.GPUReasonSetupFailed,
		LastHeartbeatTime:  transitioned,
		LastTransitionTime: transitioned,
	}}

	// A later start with the same outcome refreshes the heartbeat only.
	later := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	require.True(t, setGPUReadyCondition(&conditions, &goalstates.GPUStatus{Reason: goalstates.GPUReasonSetupFailed, Time: later}))
	require.True(t, conditions[0].LastTransitionTime.Equal(&transitioned))
	require.True(t, conditions[0].LastHeartbeatTime.Time.Equal(later))
}

func TestTruncateGPUConditionMessage(t *testing.T) {
	t.Parallel()

	require.Equal(t, "short", truncateGPUConditionMessage("short"))

	got := truncateGPUConditionMessage(strings.Repeat("x", gpuConditionMessageMaxLen-4) + "ééé")
	require.LessOrEqual(t, len(got), gpuConditionMessageMaxLen)
	require.True(t, strings.HasSuffix(got, "..."))
	require.NotContains(t, got, "\ufffd")
}

func TestSetupGPUConditionControllerValidatesInputs(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, SetupGPUConditionController(nil, "", goalstates.MachinesDir), "node name")
	require.ErrorContains(t, SetupGPUConditionController(nil, gpuTestNode, ""), "machines directory")
}
