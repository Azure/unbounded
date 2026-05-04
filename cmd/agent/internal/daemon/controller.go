// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
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
	log         *slog.Logger
	machineName string
	nodeName    string
}

func runController(ctx context.Context, log *slog.Logger, restCfg *rest.Config, _ client.WithWatch, active *ActiveMachine) error {
	nodeName, err := resolveNodeName(active.Config.MachineName)
	if err != nil {
		return err
	}

	reconciler := &daemonReconciler{
		log:         log,
		machineName: active.Config.MachineName,
		nodeName:    nodeName,
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
					Field: fields.OneTermEqualSelector("metadata.name", active.Config.MachineName),
				},
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

func resolveNodeName(machineName string) (string, error) {
	// TODO: The daemon currently assumes the Kubernetes Node name matches the
	// host hostname. We should decide the node name in the daemon and pass it
	// into kubelet config later so node naming and Machine references are
	// deterministic.
	nodeName, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("get hostname for Machine %s Node source: %w", machineName, err)
	}
	if nodeName == "" {
		return "", fmt.Errorf("get hostname for Machine %s Node source: hostname is empty", machineName)
	}

	return nodeName, nil
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
		r.log.Info("machine operation queued", "operation", req.Name)
		return reconcile.Result{}, nil
	case queueItemRepave:
		r.log.Info("repave queued", "node", req.Name)
		return reconcile.Result{}, nil
	default:
		return reconcile.Result{}, nil
	}
}

func (r *daemonReconciler) mapMachineOperation(ctx context.Context, obj client.Object) []daemonRequest {
	op, ok := obj.(*v1alpha3.MachineOperation)
	if !ok || !r.shouldEnqueueOperation(ctx, op) {
		return nil
	}

	return []daemonRequest{{Kind: queueItemMachineOperation, Name: op.Name}}
}

func (r *daemonReconciler) mapNode(_ context.Context, obj client.Object) []daemonRequest {
	if obj.GetName() != r.nodeName {
		return nil
	}

	return []daemonRequest{{Kind: queueItemRepave, Name: obj.GetName()}}
}

func (r *daemonReconciler) shouldEnqueueOperation(ctx context.Context, op *v1alpha3.MachineOperation) bool {
	if op.Status.Phase != "" && op.Status.Phase != v1alpha3.OperationPhasePending {
		return false
	}

	switch op.Spec.OperationKind {
	case v1alpha3.OperationNodeReboot, v1alpha3.OperationAgentUpgrade:
		// handled below
	default:
		return false
	}

	matches, err := r.matchesMachine(ctx, op)
	if err != nil {
		r.log.Warn("invalid MachineOperation target selector", "operation", op.Name, "error", err)
		return false
	}

	return matches
}

func (r *daemonReconciler) matchesMachine(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	if op.Spec.MachineRef != "" {
		return op.Spec.MachineRef == r.machineName, nil
	}

	if op.Spec.MachineSelector == nil {
		return false, nil
	}

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: r.nodeName}, &node); err == nil {
		return selectorMatches(op.Spec.MachineSelector, node.Labels)
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get Node %s for selector match: %w", r.nodeName, err)
	}

	var machine v1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: r.machineName}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get Machine %s for selector match: %w", r.machineName, err)
	}

	return selectorMatches(op.Spec.MachineSelector, machine.Labels)
}

func selectorMatches(selector *metav1.LabelSelector, targetLabels map[string]string) (bool, error) {
	compiled, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, err
	}

	return compiled.Matches(labels.Set(targetLabels)), nil
}

var _ reconcile.TypedReconciler[daemonRequest] = (*daemonReconciler)(nil)
