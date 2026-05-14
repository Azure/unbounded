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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type repaveReconciler struct {
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

	c := mgr.GetClient()
	machineOperations := &machineOperationTarget{
		Client:       c,
		log:          log,
		machineName:  machineName,
		nodeOperator: nodeOperator,
	}

	machineOperationReconciler, err := daemon.NewMachinaMachineOperationReconciler(
		c,
		machineName,
		nodeName,
		daemon.MachineOperationHandlers{
			v1alpha3.OperationNodeReboot:   machineOperations.reconcileNodeReboot,
			v1alpha3.OperationAgentUpgrade: machineOperations.reconcileAgentUpgrade,
			v1alpha3.OperationAgentReset:   machineOperations.reconcileAgentReset,
		},
	)
	if err != nil {
		return fmt.Errorf("create MachineOperation reconciler: %w", err)
	}
	repaveReconciler := &repaveReconciler{
		Client:       c,
		log:          log,
		machineName:  machineName,
		nodeName:     nodeName,
		nodeOperator: nodeOperator,
	}

	if err := daemon.SetupController(
		"unbounded-agent-daemon",
		mgr,
		machineOperationReconciler,
		repaveReconciler,
	); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}

	err = mgr.Start(ctx)
	log.Info("daemon shutting down")

	return err
}

func (r *repaveReconciler) SetupController(b *builder.TypedBuilder[daemon.Request]) *builder.TypedBuilder[daemon.Request] {
	mapNode := func(_ context.Context, obj client.Object) []daemon.Request {
		if obj.GetName() != r.nodeName {
			return nil
		}

		return []daemon.Request{daemon.NewRepaveRequest("node-delete")}
	}

	return b.Watches(
		&corev1.Node{},
		handler.TypedEnqueueRequestsFromMapFunc(mapNode),
		builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return false },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			DeleteFunc:  func(e event.DeleteEvent) bool { return e.Object.GetName() == r.nodeName },
			GenericFunc: func(event.GenericEvent) bool { return false },
		}),
	)
}

var _ daemon.RepaveReconciler = (*repaveReconciler)(nil)
