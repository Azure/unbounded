// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	dockerServiceUnit     = "docker.service"
	dockerSocketUnit      = "docker.socket"
	containerdServiceUnit = "containerd.service"
	kubeletServiceUnit    = "kubelet.service"
)

type disableSystemdUnits struct {
	name     string
	units    []string
	log      *slog.Logger
	runCmdAt func(context.Context, *slog.Logger, slog.Level, func(context.Context) *exec.Cmd, ...string) error
}

// DisableDocker returns a task that stops and masks Docker on the host.
func DisableDocker(log *slog.Logger) phases.Task {
	return newDisableSystemdUnits("disable-docker", log, dockerSocketUnit, dockerServiceUnit)
}

// DisableContainerd returns a task that stops and masks containerd on the host.
func DisableContainerd(log *slog.Logger) phases.Task {
	return newDisableSystemdUnits("disable-containerd", log, containerdServiceUnit)
}

// DisableKubelet returns a task that stops and masks kubelet on the host.
func DisableKubelet(log *slog.Logger) phases.Task {
	return newDisableSystemdUnits("disable-kubelet", log, kubeletServiceUnit)
}

func newDisableSystemdUnits(name string, log *slog.Logger, units ...string) phases.Task {
	return &disableSystemdUnits{name: name, units: units, log: log, runCmdAt: executil.RunCmdAt}
}

func (d *disableSystemdUnits) Name() string { return d.name }

func (d *disableSystemdUnits) Do(ctx context.Context) error {
	if err := d.ensureDisabled(ctx); err != nil {
		return fmt.Errorf("%s: %w", d.name, err)
	}

	return nil
}

func (d *disableSystemdUnits) ensureDisabled(ctx context.Context) error {
	systemctl := executil.Systemctl()

	for _, unit := range d.units {
		// Stop the unit if running. The service may not be installed, so log stop
		// errors at Debug - a missing unit is expected and not a problem.
		if err := d.runCmdAt(ctx, d.log, slog.LevelDebug, systemctl, "stop", unit); err != nil {
			d.log.DebugContext(ctx, "stopping unit (may already be stopped or not installed)", "unit", unit, "err", err)
		}

		// Mask the unit to prevent future activation. systemctl writes
		// informational messages (e.g. "Created symlink ...") to stderr on
		// success, so use Info level to avoid misleading Error logs.
		if err := d.runCmdAt(ctx, d.log, slog.LevelInfo, systemctl, "mask", unit); err != nil {
			return fmt.Errorf("masking %s: %w", unit, err)
		}
	}

	return nil
}
