// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type queueItemKind string

const (
	queueItemMachineOperation queueItemKind = "machineOperation"
	queueItemRepave           queueItemKind = "repave"
)

type daemonRequest struct {
	Kind queueItemKind
	Name string
}

type daemonReconciler struct {
	client.Client
	log          *slog.Logger
	machineName  string
	nodeName     string
	nodeOperator nodeOperator
}

func runController(
	ctx context.Context,
	log *slog.Logger,
	restCfg *rest.Config,
	machineName string,
	nodeName string,
	nodeOperator nodeOperator,
) error {
	reconciler := &daemonReconciler{
		log:          log,
		machineName:  machineName,
		nodeName:     nodeName,
		nodeOperator: nodeOperator,
	}

	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: newScheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: ctrlcache.Options{
			// Keep the daemon cache intentionally small. The daemon only needs
			// the local Node for node-label selector matching and delete-triggered
			// repave, plus the local Machine as fallback selector state before the
			// Node has joined.
			ByObject: map[client.Object]ctrlcache.ByObject{
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", nodeName),
				},
				&v1alpha3.Machine{}: {
					Field: fields.OneTermEqualSelector("metadata.name", machineName),
				},
				&v1alpha3.MachineConfigurationVersion{}: {},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create daemon manager: %w", err)
	}

	reconciler.Client = mgr.GetClient()

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}

	err = mgr.Start(ctx)
	log.Info("daemon shutting down")

	return err
}

func (r *daemonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return builder.TypedControllerManagedBy[daemonRequest](mgr).
		Named("unbounded-agent-daemon").
		// MachineOperation events drive explicit daemon operations. The mapper
		// filters to local-machine NodeReboot/AgentUpgrade operations and emits
		// typed daemon requests rather than Kubernetes object keys.
		Watches(
			&v1alpha3.MachineOperation{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapMachineOperation),
		).
		// Node delete events drive repave. The cache is already scoped to the
		// local Node name, and the predicate ignores create/update/generic events
		// so only deletion is converted into a repave request.
		Watches(
			&corev1.Node{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapNode),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				DeleteFunc:  func(e event.DeleteEvent) bool { return e.Object.GetName() == r.nodeName },
				GenericFunc: func(event.GenericEvent) bool { return false },
			}),
		).
		WithOptions(controller.TypedOptions[daemonRequest]{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *daemonReconciler) Reconcile(ctx context.Context, req daemonRequest) (reconcile.Result, error) {
	switch req.Kind {
	case queueItemMachineOperation:
		return r.reconcileMachineOperation(ctx, req.Name)
	case queueItemRepave:
		return r.reconcileRepave(ctx)
	default:
		return reconcile.Result{}, nil
	}
}

func (r *daemonReconciler) mapNode(_ context.Context, obj client.Object) []daemonRequest {
	if obj.GetName() != r.nodeName {
		return nil
	}

	return []daemonRequest{{Kind: queueItemRepave, Name: obj.GetName()}}
}

var _ reconcile.TypedReconciler[daemonRequest] = (*daemonReconciler)(nil)
