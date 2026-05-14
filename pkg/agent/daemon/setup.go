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

// SetupController registers a serialized controller with mgr. The reconciler
// owns request handling, including any goal resolution and host-local operation
// execution. All queued requests are serialized because host-local daemon
// operations mutate the same nspawn, systemd, filesystem, and daemon-binary
// state.
func SetupController(
	name string,
	mgr ctrl.Manager,
	machineOperations MachineOperationRequestReconciler,
	repaves RepaveReconciler,
) error {
	if name == "" {
		return fmt.Errorf("controller name is required")
	}
	if machineOperations == nil {
		return fmt.Errorf("machine operation reconciler is required")
	}
	if repaves == nil {
		return fmt.Errorf("repave reconciler is required")
	}

	runtimeReconciler := &controllerRuntimeReconciler{machineOperations: machineOperations, repaves: repaves}
	b := builder.TypedControllerManagedBy[Request](mgr).Named(name)
	b = machineOperations.SetupController(b)
	b = repaves.SetupController(b)

	return b.WithOptions(controller.TypedOptions[Request]{
		// Host-local operations share mutable node state, including systemd units,
		// nspawn machines, local files, and daemon binaries. Serialize requests so
		// repave, reset, restart, and upgrade flows cannot interleave.
		MaxConcurrentReconciles: 1,
	}).Complete(runtimeReconciler)
}

type controllerRuntimeReconciler struct {
	machineOperations MachineOperationRequestReconciler
	repaves           RepaveReconciler
}

func (r *controllerRuntimeReconciler) Reconcile(
	ctx context.Context,
	req Request,
) (reconcile.Result, error) {
	if operationReq, ok := req.machineOperationRequest(); ok {
		return r.machineOperations.ReconcileMachineOperation(ctx, operationReq.Name)
	}
	if repaveReq, ok := req.repaveRequest(); ok {
		return r.repaves.ReconcileRepave(ctx, repaveReq.Source)
	}

	log.FromContext(ctx).Error(nil, "ignoring invalid daemon request")

	return ctrl.Result{}, nil
}
