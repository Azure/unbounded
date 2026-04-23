// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cloudinit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	// cloudInitTimeout is the maximum duration the CloudInitDone condition
	// may remain in a non-terminal state (False/Running) before the
	// controller marks it Unknown to signal that cloud-init appears stalled.
	cloudInitTimeout = 5 * time.Minute
)

type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("cloudinit").
		For(&v1alpha3.Machine{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("controller", "cloudinit", "node", req.Name, "namespace", req.Namespace)

	var machine v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cond := meta.FindStatusCondition(machine.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)

	// No condition yet, or status is not False (already terminal) -
	// nothing to do.
	if cond == nil || cond.Status != metav1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	// The condition is False. Only time out non-terminal reasons;
	// "Failed" is already terminal.
	if cond.Reason == "Failed" {
		return ctrl.Result{}, nil
	}

	// Guard against zero LastTransitionTime to avoid a spurious
	// immediate timeout.
	if cond.LastTransitionTime.IsZero() {
		return ctrl.Result{RequeueAfter: cloudInitTimeout}, nil
	}

	elapsed := time.Since(cond.LastTransitionTime.Time)
	if elapsed < cloudInitTimeout {
		return ctrl.Result{RequeueAfter: cloudInitTimeout - elapsed}, nil
	}

	log.Info("cloud-init timed out, marking condition Unknown",
		"reason", cond.Reason, "elapsed", elapsed)

	meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionCloudInitDone,
		Status:             metav1.ConditionUnknown,
		Reason:             "TimedOut",
		Message:            fmt.Sprintf("cloud-init did not complete within %s", cloudInitTimeout),
		ObservedGeneration: machine.Generation,
	})

	return ctrl.Result{}, r.Client.Status().Update(ctx, &machine)
}
