// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// NVIDIAReconcileOptions describes the NVIDIA GPUs found at a machine start
// and whether the node requires them.
type NVIDIAReconcileOptions struct {
	// Assessment classifies the host's NVIDIA GPUs for this start.
	Assessment goalstates.NVIDIAAssessment

	// Required reports whether AgentConfig.RequiredHardware lists NVIDIA GPUs.
	Required bool
}

type reconcileNVIDIA struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
	opts      NVIDIAReconcileOptions
	now       func() time.Time
	run       func(context.Context, *slog.Logger, string, ...string) (string, error)
	execute   func(context.Context, *slog.Logger, phases.Task) error
}

// ReconcileNVIDIA returns the reusable post-start flow for an nspawn GPU node.
//
// When the GPUs can be used, it stops the consumers of the ephemeral driver
// root, rebuilds NVIDIA state, enables the NVIDIA containerd runtime, and
// restores containerd and kubelet. When they cannot be used, or setup fails,
// the node starts without them: the NVIDIA runtime drop-in and the generated
// CDI specs are removed, so containerd runs as it does on a CPU-only node.
// Either way the outcome is recorded for the GPUReady node condition. When the
// GPUs are required, unusable GPUs or a failed setup are an error instead. A
// host without NVIDIA GPUs that does not require them is left alone.
//
// goalState.Nvidia must hold opts.Assessment.Host, with Required set, when the
// assessment is usable.
func ReconcileNVIDIA(log *slog.Logger, goalState *goalstates.NodeStart, opts NVIDIAReconcileOptions) phases.Task {
	return &reconcileNVIDIA{
		log:       log,
		goalState: goalState,
		opts:      opts,
		now:       time.Now,
		run:       executil.MachineRun,
		execute:   phases.ExecuteTask,
	}
}

func (r *reconcileNVIDIA) Name() string { return "reconcile-nvidia" }

func (r *reconcileNVIDIA) Do(ctx context.Context) error {
	if !r.opts.Assessment.Usable {
		return r.startWithoutUsableGPUs(ctx)
	}

	if err := r.stopService(ctx, goalstates.SystemdUnitKubelet); err != nil {
		return err
	}

	if err := r.stopService(ctx, goalstates.SystemdUnitContainerd); err != nil {
		return err
	}

	setup := SetupNVIDIA(r.log, r.goalState)
	if setupErr := r.execute(ctx, r.log, setup); setupErr != nil {
		if err := r.setupFailed(ctx, fmt.Errorf("%s: %w", setup.Name(), setupErr)); err != nil {
			return err
		}
	} else if err := r.setupSucceeded(); err != nil {
		return err
	}

	for _, task := range []phases.Task{
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

// setupSucceeded enables the NVIDIA containerd runtime and records the GPUs as
// ready. The drop-in is written here, not only when containerd is configured,
// because a start after one that could not set up the GPUs has it removed.
func (r *reconcileNVIDIA) setupSucceeded() error {
	if _, err := writeNVIDIARuntimeDropIn(r.goalState.MachineDir, r.goalState.Containerd.NvidiaRuntime); err != nil {
		return fmt.Errorf("enable NVIDIA containerd runtime: %w", err)
	}

	r.writeStatus(goalstates.GPUStatus{
		Ready:   true,
		Reason:  goalstates.GPUReasonSetupSucceeded,
		Message: "NVIDIA GPU setup succeeded",
		Time:    r.now(),
	})

	return nil
}

// setupFailed records a failed setup. A node that requires the GPUs fails to
// start. Otherwise the GPU configuration is removed so containerd starts as it
// does on a CPU-only node.
func (r *reconcileNVIDIA) setupFailed(ctx context.Context, setupErr error) error {
	r.writeStatus(goalstates.GPUStatus{
		Reason:  goalstates.GPUReasonSetupFailed,
		Message: setupErr.Error(),
		Time:    r.now(),
	})

	if r.opts.Required {
		return fmt.Errorf("%w: %s: %w", goalstates.ErrRequiredHardwareUnavailable, config.HardwareNvidiaGPU, setupErr)
	}

	r.log.Warn("NVIDIA GPU setup failed; starting the node without GPUs",
		"machine", r.goalState.MachineName,
		"error", setupErr,
	)

	if _, err := r.removeGPUConfig(); err != nil {
		return err
	}

	r.removeDriverState(ctx)

	return nil
}

// startWithoutUsableGPUs handles a start whose GPUs cannot be set up. It
// records why and, unless the node requires the GPUs, makes sure containerd
// runs without the NVIDIA runtime that an earlier start may have enabled.
func (r *reconcileNVIDIA) startWithoutUsableGPUs(ctx context.Context) error {
	assessment := r.opts.Assessment

	if !assessment.Present && !r.opts.Required {
		r.log.Info("no NVIDIA devices discovered; skipping NVIDIA rewiring", "machine", r.goalState.MachineName)

		// Forget the outcome of an earlier start that had GPUs, so the node
		// does not keep reporting it.
		if err := goalstates.RemoveGPUStatus(r.goalState.MachineDir); err != nil {
			r.log.Warn("failed to remove stale GPU status", "machine", r.goalState.MachineName, "error", err)
		}

		return nil
	}

	r.writeStatus(assessment.UnavailableStatus(r.now()))

	if err := assessment.Require(r.opts.Required); err != nil {
		return err
	}

	r.log.Warn("NVIDIA GPUs are present but cannot be set up; starting the node without them",
		"machine", r.goalState.MachineName,
		"reason", assessment.Reason,
		"message", assessment.Message,
	)

	removed, err := r.removeGPUConfig()
	if err != nil || !removed {
		return err
	}

	// containerd may already be running with the NVIDIA runtime an earlier
	// start enabled. try-restart leaves it alone when it is not running, as on
	// the first start of a new machine.
	if _, err := r.run(ctx, r.log, r.goalState.MachineName,
		"systemctl", "try-restart", goalstates.SystemdUnitContainerd,
	); err != nil {
		return fmt.Errorf("restart %s in %s: %w", goalstates.SystemdUnitContainerd, r.goalState.MachineName, err)
	}

	return nil
}

// removeGPUConfig removes the NVIDIA runtime drop-in and the CDI specs that GPU
// setup generates from the machine rootfs, so containerd runs as it does on a
// CPU-only node. It reports whether anything was removed.
func (r *reconcileNVIDIA) removeGPUConfig() (bool, error) {
	removed, err := removeNVIDIARuntimeDropIn(r.goalState.MachineDir)
	if err != nil {
		return false, fmt.Errorf("remove NVIDIA containerd runtime config: %w", err)
	}

	for _, spec := range nvidiaCDISpecFiles() {
		specRemoved, err := removeFileIfPresent(filepath.Join(r.goalState.MachineDir, spec))
		if err != nil {
			return false, fmt.Errorf("remove NVIDIA CDI spec %s: %w", spec, err)
		}

		removed = removed || specRemoved
	}

	return removed, nil
}

// nvidiaCDISpecFiles returns the in-machine CDI spec files nvidia-ctk writes
// for goalstates.CDISpecFile. On systems with both coherent and non-coherent
// GPUs it also writes specs with those infixes.
func nvidiaCDISpecFiles() []string {
	ext := filepath.Ext(goalstates.CDISpecFile)
	base := strings.TrimSuffix(goalstates.CDISpecFile, ext)

	return []string{
		goalstates.CDISpecFile,
		base + ".coherent" + ext,
		base + ".noncoherent" + ext,
	}
}

// removeDriverState removes the driver root and library links that a failed
// setup may have left inside the machine. Failures are logged rather than
// returned: with the NVIDIA runtime removed, nothing on the node uses them.
func (r *reconcileNVIDIA) removeDriverState(ctx context.Context) {
	commands := [][]string{{"rm", "-rf", goalstates.NvidiaDriverDir}}

	if libDir := r.goalState.Nvidia.ContainerLibDir; libDir != "" {
		commands = append(commands,
			[]string{"find", libDir, "-maxdepth", "1", "-lname", goalstates.NvidiaHostLibDir + "/*", "-delete"},
			[]string{"ldconfig"},
		)
	}

	for _, args := range commands {
		if _, err := r.run(ctx, r.log, r.goalState.MachineName, args...); err != nil {
			r.log.Warn("failed to clean up NVIDIA state after failed setup",
				"machine", r.goalState.MachineName,
				"command", strings.Join(args, " "),
				"error", err,
			)
		}
	}
}

// writeStatus records this start's GPU outcome for the GPUReady node
// condition. A failure is logged rather than returned, because it must not stop
// the node from starting.
func (r *reconcileNVIDIA) writeStatus(status goalstates.GPUStatus) {
	if err := goalstates.WriteGPUStatus(r.goalState.MachineDir, status); err != nil {
		r.log.Warn("failed to record GPU status", "machine", r.goalState.MachineName, "error", err)
	}
}

func (r *reconcileNVIDIA) stopService(ctx context.Context, service string) error {
	if _, err := r.run(ctx, r.log, r.goalState.MachineName, "systemctl", "stop", service); err != nil {
		return fmt.Errorf("stop %s in %s: %w", service, r.goalState.MachineName, err)
	}

	return nil
}
