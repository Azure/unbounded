// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package redfish

import (
	"context"
	"fmt"
	"log/slog"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

type Reconciler struct {
	Client       client.Client
	Pool         *Pool
	FileResolver *netboot.FileResolver
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("redfish").
		For(&v1alpha3.Machine{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				m, ok := e.Object.(*v1alpha3.Machine)
				return ok && m.Spec.PXE != nil && m.Spec.PXE.Redfish != nil
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				m, ok := e.ObjectNew.(*v1alpha3.Machine)
				return ok && m.Spec.PXE != nil && m.Spec.PXE.Redfish != nil
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				m, ok := e.Object.(*v1alpha3.Machine)
				return ok && m.Spec.PXE != nil && m.Spec.PXE.Redfish != nil
			},
		}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := slog.With("node", req.Name, "namespace", req.Namespace)

	var machine v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if machine.Spec.PXE == nil || machine.Spec.PXE.Redfish == nil {
		return ctrl.Result{}, nil
	}

	rf := machine.Spec.PXE.Redfish

	// TOFU: capture TLS cert fingerprint on first connection.
	fingerprint := ""
	if machine.Status.Redfish != nil {
		fingerprint = machine.Status.Redfish.CertFingerprint
	}

	if fingerprint == "" {
		fp, err := CaptureFingerprint(ctx, rf.URL)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("capturing TLS cert fingerprint: %w", err)
		}

		if machine.Status.Redfish == nil {
			machine.Status.Redfish = &v1alpha3.RedfishStatus{}
		}

		machine.Status.Redfish.CertFingerprint = fp
		logger.InfoContext(ctx, "TOFU: captured TLS cert fingerprint", "fingerprint", fp)

		return ctrl.Result{}, r.Client.Status().Update(ctx, &machine)
	}

	return ctrl.Result{}, nil
}
