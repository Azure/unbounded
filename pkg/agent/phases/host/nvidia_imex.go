// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type prepareNVIDIAIMEXDRA struct {
	log *slog.Logger
	cfg *config.AgentConfig
}

// PrepareNVIDIAIMEXDRA installs the branch-matched Ubuntu IMEX package and
// reserves daemon ownership for NVIDIA DRA. It is a no-op unless DRA mode is
// explicitly selected.
func PrepareNVIDIAIMEXDRA(log *slog.Logger, cfg *config.AgentConfig) phases.Task {
	return &prepareNVIDIAIMEXDRA{log: log, cfg: cfg}
}

func (p *prepareNVIDIAIMEXDRA) Name() string { return "prepare-nvidia-imex-dra" }

func (p *prepareNVIDIAIMEXDRA) Do(ctx context.Context) error {
	if !p.cfg.IMEXDRAEnabled() {
		return nil
	}

	if err := p.cfg.Validate(); err != nil {
		return fmt.Errorf("validate NVIDIA IMEX DRA configuration: %w", err)
	}

	if err := goalstates.ValidateNVIDIAIMEXKubernetesVersion(p.cfg.Cluster.Version); err != nil {
		return err
	}

	if err := validateNVIDIAIMEXHostOS("/etc/os-release"); err != nil {
		return err
	}

	pm, err := detectHostPackageManager(exec.LookPath)
	if err != nil {
		return fmt.Errorf("detect package manager for NVIDIA IMEX: %w", err)
	}

	if pm.name != "apt-get" {
		return fmt.Errorf("NVIDIA IMEX DRA package installation is supported only on Ubuntu apt hosts, found %s", pm.name)
	}

	nvidia, err := goalstates.ResolveNvidiaHost(runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("inspect NVIDIA host before IMEX preparation: %w", err)
	}

	driverMajor, err := validateNVIDIAIMEXPreinstallHost(nvidia)
	if err != nil {
		return err
	}

	packageWasPresent := nvidia.NvidiaIMEXPath != "" && nvidia.NvidiaIMEXCtlPath != ""

	if err := maskNVIDIAIMEXService(ctx, p.log); err != nil {
		return err
	}

	pkg := fmt.Sprintf("nvidia-imex-%d", driverMajor)
	if err := InstallPackages(p.log, pkg).Do(ctx); err != nil {
		if !packageWasPresent {
			if cleanupErr := executil.RunCmd(ctx, p.log, executil.Systemctl(), "unmask", "nvidia-imex.service"); cleanupErr != nil {
				p.log.Warn("failed to remove temporary NVIDIA IMEX service mask", "error", cleanupErr)
			}
		}

		return fmt.Errorf("install NVIDIA IMEX package %s: %w", pkg, err)
	}

	if err := executil.RunCmd(ctx, p.log, executil.Systemctl(), "disable", "--now", "nvidia-imex.service"); err != nil {
		if maskErr := maskNVIDIAIMEXService(ctx, p.log); maskErr != nil {
			p.log.Warn("failed to preserve NVIDIA IMEX service mask after disable failure", "error", maskErr)
		}

		return fmt.Errorf("disable host nvidia-imex service for DRA ownership: %w", err)
	}

	if err := maskNVIDIAIMEXService(ctx, p.log); err != nil {
		return err
	}

	return nil
}

func maskNVIDIAIMEXService(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "mask", "nvidia-imex.service"); err != nil {
		return fmt.Errorf("mask host nvidia-imex service for DRA ownership: %w", err)
	}

	return nil
}

func validateNVIDIAIMEXPreinstallHost(nvidia goalstates.NvidiaHost) (int, error) {
	if len(nvidia.GPUDevicePaths) == 0 {
		return 0, fmt.Errorf("NVIDIA IMEX DRA requires NVIDIA GPU devices")
	}

	if !goalstates.NVIDIAStateAvailable(nvidia) {
		return 0, fmt.Errorf("NVIDIA IMEX DRA requires complete host driver libraries and version state")
	}

	if !nvidia.IMEXManagementCapability {
		return 0, fmt.Errorf("NVIDIA IMEX DRA requires /proc/driver/nvidia/capabilities/fabric-imex-mgmt")
	}

	if !nvidia.IMEXDeviceClass {
		return 0, fmt.Errorf("NVIDIA IMEX DRA requires the nvidia-caps-imex-channels character-device class")
	}

	if !slices.Contains(nvidia.GPUDevicePaths, "/dev/nvidia-caps") {
		return 0, fmt.Errorf("NVIDIA IMEX DRA requires /dev/nvidia-caps")
	}

	driverMajor, err := parseNVIDIADriverMajor(nvidia.DriverVersion)
	if err != nil {
		return 0, err
	}

	if driverMajor < 580 {
		return 0, fmt.Errorf("NVIDIA IMEX DRA requires driver 580 or later, found %q", nvidia.DriverVersion)
	}

	return driverMajor, nil
}

func validateNVIDIAIMEXHostOS(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read host OS release for NVIDIA IMEX DRA: %w", err)
	}

	values := make(map[string]string)

	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}

		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}

	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" {
		return fmt.Errorf(
			"NVIDIA IMEX DRA host preparation supports Ubuntu 24.04 only, found ID=%q VERSION_ID=%q",
			values["ID"],
			values["VERSION_ID"],
		)
	}

	return nil
}

func parseNVIDIADriverMajor(version string) (int, error) {
	majorText, _, _ := strings.Cut(strings.TrimSpace(version), ".")

	major, err := strconv.Atoi(majorText)
	if err != nil {
		return 0, fmt.Errorf("parse NVIDIA driver version %q: %w", version, err)
	}

	return major, nil
}
