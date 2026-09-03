// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type reconcileNVIDIA struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
	run       func(context.Context, *slog.Logger, string, ...string) (string, error)
	execute   func(context.Context, *slog.Logger, phases.Task) error
}

// ReconcileNVIDIA returns the reusable post-start flow for an nspawn GPU node.
// It stops consumers of the ephemeral driver root, rebuilds NVIDIA state, then
// restores containerd and kubelet. Non-GPU discovery is a no-op.
func ReconcileNVIDIA(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &reconcileNVIDIA{
		log:       log,
		goalState: goalState,
		run:       executil.MachineRun,
		execute:   phases.ExecuteTask,
	}
}

func (r *reconcileNVIDIA) Name() string { return "reconcile-nvidia" }

func (r *reconcileNVIDIA) Do(ctx context.Context) error {
	if !r.goalState.Nvidia.Required {
		r.log.Info("no NVIDIA devices discovered; skipping NVIDIA rewiring", "machine", r.goalState.MachineName)

		return nil
	}

	if err := r.stopService(ctx, goalstates.SystemdUnitKubelet); err != nil {
		return err
	}

	if err := r.stopService(ctx, goalstates.SystemdUnitContainerd); err != nil {
		return err
	}

	for _, task := range []phases.Task{
		SetupNVIDIA(r.log, r.goalState),
		StartContainerd(r.log, r.goalState),
		ImportContainerImages(r.log, r.goalState),
		StartKubelet(r.log, r.goalState),
	} {
		if err := r.execute(ctx, r.log, task); err != nil {
			return fmt.Errorf("%s: %w", task.Name(), err)
		}
	}

	return nil
}

func (r *reconcileNVIDIA) stopService(ctx context.Context, service string) error {
	if _, err := r.run(ctx, r.log, r.goalState.MachineName, "systemctl", "stop", service); err != nil {
		return fmt.Errorf("stop %s in %s: %w", service, r.goalState.MachineName, err)
	}

	return nil
}
