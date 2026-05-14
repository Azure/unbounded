// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SetupWithManager registers a serialized controller with mgr. The reconciler
// owns request handling, including any goal resolution and host-local operation
// execution. All queued requests are serialized because host-local daemon
// operations mutate the same nspawn, systemd, filesystem, and daemon-binary
// state.
func SetupWithManager(
	name string,
	mgr ctrl.Manager,
	machineOperations MachineOperationReconciler,
	repaves RepaveReconciler,
	setupControllers ...SetupController,
) error {
	if name == "" {
		return fmt.Errorf("controller name is required")
	}
	if len(setupControllers) == 0 {
		return fmt.Errorf("at least one controller setup callback is required")
	}
	for i, setupController := range setupControllers {
		if setupController == nil {
			return fmt.Errorf("controller setup callback %d is required", i)
		}
	}
	if machineOperations == nil {
		return fmt.Errorf("machine operation reconciler is required")
	}
	if repaves == nil {
		return fmt.Errorf("repave reconciler is required")
	}

	runtimeReconciler := &controllerRuntimeReconciler{machineOperations: machineOperations, repaves: repaves}
	b := builder.TypedControllerManagedBy[Request](mgr).Named(name)
	for _, setupController := range setupControllers {
		b = setupController(b)
	}

	return b.WithOptions(controller.TypedOptions[Request]{MaxConcurrentReconciles: 1}).Complete(runtimeReconciler)
}

type controllerRuntimeReconciler struct {
	machineOperations MachineOperationReconciler
	repaves           RepaveReconciler
}

func (r *controllerRuntimeReconciler) Reconcile(
	ctx context.Context,
	req Request,
) (reconcile.Result, error) {
	if operationReq, ok := req.machineOperationRequest(); ok {
		return r.machineOperations.ReconcileMachineOperation(ctx, operationReq)
	}
	if repaveReq, ok := req.repaveRequest(); ok {
		return r.repaves.ReconcileRepave(ctx, repaveReq)
	}

	log.FromContext(ctx).Error(nil, "ignoring invalid daemon request")

	return ctrl.Result{}, nil
}
