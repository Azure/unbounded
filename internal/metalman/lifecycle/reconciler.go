// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	cloudInitReasonTimedOut = "TimedOut"

	repaveTimeout = 30 * time.Minute // TODO: Make this configurable
)

type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("lifecycle").
		For(&v1alpha3.Machine{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("controller", "lifecycle", "node", req.Name, "namespace", req.Namespace)

	var node v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if node.Spec.Operations == nil || node.Status.Operations == nil {
		return ctrl.Result{}, nil
	}

	if retryCloudInitTimeout(&node) {
		log.Info("cloud-init timed out after repave, triggering retry")

		return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
	}

	pendingRepave := node.Spec.Operations.RepaveCounter > node.Status.Operations.RepaveCounter
	if !pendingRepave {
		return ctrl.Result{}, nil
	}

	repavedCond := meta.FindStatusCondition(node.Status.Conditions, v1alpha3.MachineConditionRepaved)
	if repavedCond == nil || repavedCond.Status != metav1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	elapsed := time.Since(repavedCond.LastTransitionTime.Time)
	if elapsed < repaveTimeout {
		return ctrl.Result{RequeueAfter: repaveTimeout - elapsed}, nil
	}

	log.Info("repave timed out, triggering retry", "elapsed", elapsed)
	retryRepaveBoot(&node)

	return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
}

func retryCloudInitTimeout(node *v1alpha3.Machine) bool {
	cloudInitCond := meta.FindStatusCondition(node.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cloudInitCond == nil || cloudInitCond.Status != metav1.ConditionUnknown || cloudInitCond.Reason != cloudInitReasonTimedOut {
		return false
	}

	if node.Spec.Operations.RebootCounter <= 0 || node.Spec.Operations.RepaveCounter <= 0 {
		return false
	}

	if node.Status.Operations.RebootCounter < node.Spec.Operations.RebootCounter ||
		node.Status.Operations.RepaveCounter < node.Spec.Operations.RepaveCounter {
		return false
	}

	meta.RemoveStatusCondition(&node.Status.Conditions, v1alpha3.MachineConditionRepaved)
	meta.RemoveStatusCondition(&node.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	node.Status.Operations.RebootCounter = node.Spec.Operations.RebootCounter - 1
	node.Status.Operations.RepaveCounter = node.Spec.Operations.RepaveCounter - 1

	return true
}

func retryRepaveBoot(node *v1alpha3.Machine) {
	meta.RemoveStatusCondition(&node.Status.Conditions, v1alpha3.MachineConditionRepaved)

	if node.Status.Operations.RebootCounter > 0 {
		node.Status.Operations.RebootCounter--
	}
}
