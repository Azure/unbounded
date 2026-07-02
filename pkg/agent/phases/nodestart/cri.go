// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const (
	nvidiaRuntimeDropInName = "99-nvidia-runtime.toml"
)

type configureContainerd struct {
	goalState *goalstates.NodeStart
}

// ConfigureContainerd returns a task that writes the containerd configuration, systemd unit,
// and optional GPU drop-in configs into the machine rootfs. It runs before the nspawn machine
// is started, so all paths are relative to the machine directory on the host filesystem.
func ConfigureContainerd(goalState *goalstates.NodeStart) phases.Task {
	return &configureContainerd{goalState: goalState}
}

func (c *configureContainerd) Name() string { return "configure-containerd" }

func (c *configureContainerd) Do(_ context.Context) error {
	if err := c.ensureContainerdConfig(); err != nil {
		return fmt.Errorf("ensure containerd config: %w", err)
	}

	if err := c.ensureContainerdServiceUnit(); err != nil {
		return fmt.Errorf("ensure containerd service unit: %w", err)
	}

	if err := c.ensureRegistryHosts(); err != nil {
		return fmt.Errorf("ensure registry hosts: %w", err)
	}

	if err := c.ensureGPUDropInConfigs(); err != nil {
		return fmt.Errorf("ensure GPU drop-in configs: %w", err)
	}

	return nil
}

// ensureContainerdConfig renders and writes the main containerd config.toml
// into the machine rootfs.
func (c *configureContainerd) ensureContainerdConfig() error {
	spec := c.goalState.Containerd

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, "containerd.toml", map[string]any{
		"SandboxImage":   spec.SandboxImage,
		"RuncBinaryPath": spec.RuncBinaryPath,
		"CNIBinDir":      spec.CNIBinDir,
		"CNIConfDir":     spec.CNIConfDir,
		"MetricsAddress": spec.MetricsAddress,
	}); err != nil {
		return err
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.ContainerdConfigPath)

	return utilio.WriteFile(dest, buf.Bytes(), 0o644)
}

func (c *configureContainerd) ensureRegistryHosts() error {
	for _, host := range c.goalState.Containerd.RegistryHosts {
		content := fmt.Sprintf("server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n", host.Server, host.Server)

		dest := filepath.Join(c.goalState.MachineDir, "/etc/containerd/certs.d", strings.TrimSpace(host.Host), "hosts.toml")
		if err := utilio.WriteFile(dest, []byte(content), 0o644); err != nil {
			return err
		}
	}

	return nil
}

// ensureContainerdServiceUnit renders and writes the containerd systemd unit
// file into the machine rootfs.
func (c *configureContainerd) ensureContainerdServiceUnit() error {
	spec := c.goalState.Containerd

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, "containerd.service", map[string]any{
		"ContainerdBinPath": spec.ContainerdBinPath,
	}); err != nil {
		return err
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd)

	return utilio.WriteFile(dest, buf.Bytes(), 0o644)
}

// ensureGPUDropInConfigs manages GPU-related containerd drop-in configs.
// When the nvidia runtime is enabled the drop-in is written; otherwise it is
// removed.
func (c *configureContainerd) ensureGPUDropInConfigs() error {
	nvidia := c.goalState.Containerd.NvidiaRuntime

	return ensureDropInConfig(
		c.goalState.MachineDir,
		nvidiaRuntimeDropInName,
		nvidia.Enabled,
		map[string]any{
			"RuntimePath":                nvidia.RuntimePath,
			"RuntimeClassName":           nvidia.RuntimeClassName,
			"DisableSetAsDefaultRuntime": nvidia.DisableSetAsDefaultRuntime,
		},
	)
}

type startContainerd struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// StartContainerd returns a task that enables and starts the containerd systemd service
// inside the running nspawn machine.
func StartContainerd(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &startContainerd{log: log, goalState: goalState}
}

func (s *startContainerd) Name() string { return "start-containerd" }

func (s *startContainerd) Do(ctx context.Context) error {
	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName,
		"systemctl", "enable", "--now", goalstates.SystemdUnitContainerd,
	); err != nil {
		return fmt.Errorf("systemctl enable --now %s in %s: %w",
			goalstates.SystemdUnitContainerd, s.goalState.MachineName, err)
	}

	return nil
}

// ensureDropInConfig writes or removes a containerd drop-in config file in the
// machine rootfs. If enabled is true, the template is rendered and written.
// If enabled is false, the drop-in is removed if it exists.
func ensureDropInConfig(
	machineDir string,
	dropInName string,
	enabled bool,
	templateData map[string]any,
) error {
	dropInPath := filepath.Join(machineDir, goalstates.ContainerdConfDropInDir, dropInName)

	if !enabled {
		err := os.Remove(dropInPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, dropInName, templateData); err != nil {
		return err
	}

	return utilio.WriteFile(dropInPath, buf.Bytes(), 0o644)
}
