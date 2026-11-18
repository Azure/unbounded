// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

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
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

type repaveReconciler struct {
	installation *installstate.Store
	client.Client
	// apiReader reads the API server live for the allocation launch guard.
	apiReader        client.Reader
	allocationLaunch allocationLaunchOptions
	log              *slog.Logger
	machineName      string
	nodeName         string
	nodeOperator     nodeOperator

	// env is where mode changes live on the host. Nil is the real host.
	env *migrationEnv
	// isolationMigrator carries out a mode change. Nil is env.
	isolationMigrator isolationMigrator
	// handoff is shared with the MachineOperation handlers: set once a
	// reprovision runs without this daemon, which is then about to be
	// removed. Nil is a private flag.
	handoff *handoffState
	// skipChangeTo is the target of a change a settle just resolved as a
	// failure. The repave that follows goes ahead in the current mode instead
	// of trying the change again at once; the next repave tries it.
	skipChangeTo isolation.Mode
}

// handoffState records that a reprovision took the host over from this daemon.
type handoffState struct{ handedOff atomic.Bool }

func (h *handoffState) set() {
	if h != nil {
		h.handedOff.Store(true)
	}
}

func (h *handoffState) active() bool { return h != nil && h.handedOff.Load() }

func (r *repaveReconciler) migration() *migrationEnv {
	if r.env == nil {
		r.env = &migrationEnv{}
	}

	return r.env
}

func (r *repaveReconciler) migrator() isolationMigrator {
	if r.isolationMigrator != nil {
		return r.isolationMigrator
	}

	return r.migration()
}

func (r *repaveReconciler) markHandedOff() {
	if r.handoff == nil {
		r.handoff = &handoffState{}
	}

	r.handoff.set()
}

func runController(
	ctx context.Context,
	log *slog.Logger,
	restCfg *rest.Config,
	machineName string,
	nodeName string,
	nodeOperator nodeOperator,
	installation *installstate.Store,
	handoff *handoffState,
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

	// An AgentUpgrade stops the manager with ErrRestartRequested as the cause
	// once it has staged the new binary; see managerExit.
	mgrCtx, stopManager := context.WithCancelCause(ctx)
	defer stopManager(nil)

	c := mgr.GetClient()
	machineOperations := &machineOperationTarget{
		installation:         installation,
		Client:               c,
		apiReader:            mgr.GetAPIReader(),
		allocationLaunch:     defaultAllocationLaunchOptions(),
		log:                  log,
		machineName:          machineName,
		nodeOperator:         nodeOperator,
		agentUpgradeLockPath: goalstates.DaemonAgentUpgradeLockPath,
		requestRestart:       func() { stopManager(ErrRestartRequested) },
		handoff:              handoff,
	}

	machineOperationReconciler, err := daemon.NewMachinaMachineOperationReconcilerWithReader(
		c,
		mgr.GetAPIReader(),
		machineName,
		nodeName,
		machineOperations.handlers(),
	)
	if err != nil {
		return fmt.Errorf("create MachineOperation reconciler: %w", err)
	}

	repaveReconciler := &repaveReconciler{
		installation:     installation,
		Client:           c,
		apiReader:        mgr.GetAPIReader(),
		allocationLaunch: defaultAllocationLaunchOptions(),
		log:              log,
		machineName:      machineName,
		nodeName:         nodeName,
		nodeOperator:     nodeOperator,
		handoff:          handoff,
	}

	if err := daemon.SetupController(
		"unbounded-agent-daemon",
		mgr,
		machineOperationReconciler,
		repaveReconciler,
	); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}

	err = mgr.Start(mgrCtx)

	log.Info("daemon shutting down")

	return managerExit(mgrCtx, err)
}

// managerExit returns why the daemon's manager stopped, given the context it
// ran on and what Start returned: ErrRestartRequested when the daemon stopped
// it to be restarted, and otherwise err. A manager that failed returns its
// error even then, which systemd also restarts.
func managerExit(mgrCtx context.Context, err error) error {
	if err == nil && errors.Is(context.Cause(mgrCtx), ErrRestartRequested) {
		return ErrRestartRequested
	}

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
