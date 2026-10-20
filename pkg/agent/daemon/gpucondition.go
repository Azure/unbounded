// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// NodeConditionGPUReady reports whether the node's GPUs were set up at the last
// start of its nspawn machine. Nodes on hosts without GPUs do not get it unless
// they require GPUs.
const NodeConditionGPUReady corev1.NodeConditionType = "GPUReady"

// gpuConditionMessageMaxLen bounds the condition message, which can carry the
// tail of a failed setup command's output.
const gpuConditionMessageMaxLen = 1024

// SetupGPUConditionController registers a controller with mgr that reports the
// GPU setup outcome recorded by the nspawn post-start hook as the GPUReady
// condition of the local Node, nodeName. machinesDir holds the nspawn machine
// root filesystems, normally goalstates.MachinesDir.
//
// The controller reconciles when it starts and when the Node is created or
// updated. Kubelet updates its Node as soon as it starts after a machine
// start, so the condition follows every start without polling.
//
// The daemon's credentials identify it as the node, so the Node authorizer and
// the NodeRestriction admission plugin let it update its own Node's status and
// no other.
func SetupGPUConditionController(mgr ctrl.Manager, nodeName, machinesDir string) error {
	if nodeName == "" {
		return fmt.Errorf("node name is required")
	}

	if machinesDir == "" {
		return fmt.Errorf("machines directory is required")
	}

	r := &gpuConditionReconciler{
		client:      mgr.GetClient(),
		machinesDir: machinesDir,
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("gpu-ready-condition").
		For(&corev1.Node{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == nodeName
		}))).
		Complete(r)
}

type gpuConditionReconciler struct {
	client      client.Client
	machinesDir string
}

func (r *gpuConditionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.client.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status, err := goalstates.LatestGPUStatus(r.machinesDir)
	if err != nil {
		return ctrl.Result{}, err
	}

	updated := node.DeepCopy()
	if !setGPUReadyCondition(&updated.Status.Conditions, status) {
		return ctrl.Result{}, nil
	}

	// A strategic merge patch merges conditions by type, so the conditions
	// kubelet maintains are left as they are on the server, however they have
	// changed since the Node was read.
	if err := r.client.Status().Patch(ctx, updated, client.StrategicMergeFrom(&node)); err != nil {
		return ctrl.Result{}, fmt.Errorf("update %s condition of Node %s: %w", NodeConditionGPUReady, node.Name, err)
	}

	return ctrl.Result{}, nil
}

// setGPUReadyCondition makes conditions report status, removing the GPUReady
// condition when status is nil. It reports whether conditions changed.
func setGPUReadyCondition(conditions *[]corev1.NodeCondition, status *goalstates.GPUStatus) bool {
	index := slices.IndexFunc(*conditions, func(c corev1.NodeCondition) bool {
		return c.Type == NodeConditionGPUReady
	})

	if status == nil {
		if index < 0 {
			return false
		}

		*conditions = slices.Delete(*conditions, index, index+1)

		return true
	}

	// The API server stores times to the second. Comparing at that precision
	// keeps the patch the controller makes from looking like another change.
	checked := metav1.NewTime(status.Time.Truncate(time.Second))
	desired := corev1.NodeCondition{
		Type:               NodeConditionGPUReady,
		Status:             corev1.ConditionFalse,
		Reason:             status.Reason,
		Message:            truncateGPUConditionMessage(status.Message),
		LastHeartbeatTime:  checked,
		LastTransitionTime: checked,
	}

	if status.Ready {
		desired.Status = corev1.ConditionTrue
	}

	if index < 0 {
		*conditions = append(*conditions, desired)

		return true
	}

	current := (*conditions)[index]
	if current.Status == desired.Status {
		desired.LastTransitionTime = current.LastTransitionTime
	}

	if current.Status == desired.Status &&
		current.Reason == desired.Reason &&
		current.Message == desired.Message &&
		current.LastHeartbeatTime.Equal(&desired.LastHeartbeatTime) {
		return false
	}

	(*conditions)[index] = desired

	return true
}

func truncateGPUConditionMessage(message string) string {
	if len(message) <= gpuConditionMessageMaxLen {
		return message
	}

	return strings.ToValidUTF8(message[:gpuConditionMessageMaxLen-3], "") + "..."
}
