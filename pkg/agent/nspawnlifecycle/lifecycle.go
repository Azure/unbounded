// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nspawnlifecycle provides host-side nspawn lifecycle operations that
// applications can expose through their own CLI.
package nspawnlifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// ConfigLoader returns the persisted config for a machine. found is false
// during the initial bootstrap window before the application persists config.
type ConfigLoader func(ctx context.Context, machineName string) (cfg *config.AgentConfig, found bool, err error)

// Hooks supplies application-owned lifecycle integration points.
type Hooks struct {
	LoadConfig ConfigLoader
}

// Lifecycle implements reusable pre-start, post-start, and reconcile flows.
type Lifecycle struct {
	log             *slog.Logger
	loadConfig      ConfigLoader
	resolveConfig   func(*config.AgentConfig, string) (*goalstates.RootFS, error)
	resolveNVIDIA   func(string) (goalstates.NvidiaHost, error)
	waitMachine     func(context.Context, *slog.Logger, string) error
	execute         func(context.Context, *slog.Logger, phases.Task) error
	reloadSystemd   func(context.Context, *slog.Logger) error
	getenv          func(string) string
	reconcileNVIDIA func(*slog.Logger, *goalstates.NodeStart, nodestart.NVIDIAReconcileOptions) phases.Task
}

// New returns a lifecycle implementation using shared agent behavior and the
// application's persisted-config loader.
func New(log *slog.Logger, hooks Hooks) (*Lifecycle, error) {
	if log == nil {
		return nil, fmt.Errorf("logger is required")
	}

	if hooks.LoadConfig == nil {
		return nil, fmt.Errorf("config loader is required")
	}

	return &Lifecycle{
		log:           log,
		loadConfig:    hooks.LoadConfig,
		resolveConfig: goalstates.ResolveNSpawnConfig,
		resolveNVIDIA: goalstates.ResolveNvidiaHost,
		waitMachine:   nodestart.WaitForMachine,
		execute:       phases.ExecuteTask,
		reloadSystemd: func(ctx context.Context, log *slog.Logger) error {
			return executil.RunCmd(ctx, log, executil.Systemctl(), "daemon-reload")
		},
		getenv:          os.Getenv,
		reconcileNVIDIA: nodestart.ReconcileNVIDIA,
	}, nil
}

// PreStart refreshes host-side nspawn config before the managed unit starts.
func (l *Lifecycle) PreStart(ctx context.Context, machineName string) error {
	cfg, found, err := l.loadConfig(ctx, machineName)
	if err != nil {
		return err
	}

	if !found {
		l.log.Info("applied config not available during initial bootstrap; keeping freshly provisioned nspawn config", "machine", machineName)

		return nil
	}

	rootFS, err := l.resolveConfig(cfg, machineName)
	if err != nil {
		return fmt.Errorf("resolve nspawn config goal state: %w", err)
	}

	if err := l.execute(ctx, l.log, rootfs.EnsureNSpawnConfig(l.log, rootFS)); err != nil {
		return fmt.Errorf("regenerate nspawn config for %s: %w", machineName, err)
	}

	if err := l.reloadSystemd(ctx, l.log); err != nil {
		return fmt.Errorf("reload systemd after regenerating config for %s: %w", machineName, err)
	}

	return nil
}

// PostStart sets up NVIDIA GPUs after the managed nspawn unit starts. GPUs that
// cannot be set up are left out and the node starts without them, unless the
// unit's EnvRequiredHardware lists them. Then PostStart fails, which fails the
// unit, and its restart policy tries again. Each start checks the GPUs afresh
// and records the outcome for the GPUReady node condition.
func (l *Lifecycle) PostStart(ctx context.Context, machineName string) error {
	assessment := goalstates.AssessNVIDIAHost(l.resolveNVIDIA(runtime.GOARCH))
	required := config.RequiresHardware(
		parseRequiredHardware(l.getenv(goalstates.EnvRequiredHardware)),
		config.HardwareNvidiaGPU,
	)

	// Only hosts with GPUs run commands inside the machine.
	if assessment.Present {
		if err := l.waitMachine(ctx, l.log, machineName); err != nil {
			return fmt.Errorf("wait for machine %s: %w", machineName, err)
		}
	}

	nvidia := assessment.Host
	nvidia.Required = assessment.Usable

	nodeStart := &goalstates.NodeStart{
		MachineName: machineName,
		MachineDir:  goalstates.MachineDir(machineName),
		Containerd:  goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: true}),
		Nvidia:      nvidia,
	}

	task := l.reconcileNVIDIA(l.log, nodeStart, nodestart.NVIDIAReconcileOptions{
		Assessment: assessment,
		Required:   required,
	})
	if err := l.execute(ctx, l.log, task); err != nil {
		return fmt.Errorf("reconcile NVIDIA state for %s: %w", machineName, err)
	}

	return nil
}

// parseRequiredHardware splits the comma-separated EnvRequiredHardware value.
func parseRequiredHardware(value string) []string {
	var hardware []string

	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			hardware = append(hardware, item)
		}
	}

	return hardware
}

// Reconcile restarts the managed nspawn unit, which invokes PreStart and
// PostStart through its generated systemd hooks.
func (l *Lifecycle) Reconcile(ctx context.Context, machineName string) error {
	return l.execute(ctx, l.log, nodestart.ReconcileNSpawnLifecycle(l.log, machineName))
}
