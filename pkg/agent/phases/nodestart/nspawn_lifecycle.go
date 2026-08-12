// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type reconcileNSpawnLifecycle struct {
	log         *slog.Logger
	machineName string
	restart     func(context.Context, *slog.Logger, string) error
}

// ReconcileNSpawnLifecycle returns the common host-side mini-repave flow. The
// managed nspawn unit performs config refresh in ExecStartPre and NVIDIA
// rewiring in ExecStartPost.
func ReconcileNSpawnLifecycle(log *slog.Logger, machineName string) phases.Task {
	return &reconcileNSpawnLifecycle{
		log:         log,
		machineName: machineName,
		restart: func(ctx context.Context, log *slog.Logger, unit string) error {
			return executil.RunCmd(ctx, log, executil.Systemctl(), "restart", unit)
		},
	}
}

func (r *reconcileNSpawnLifecycle) Name() string { return "reconcile-nspawn-lifecycle" }

func (r *reconcileNSpawnLifecycle) Do(ctx context.Context) error {
	unit := fmt.Sprintf("systemd-nspawn@%s.service", r.machineName)
	if err := r.restart(ctx, r.log, unit); err != nil {
		return fmt.Errorf("restart %s: %w", unit, err)
	}

	return nil
}
