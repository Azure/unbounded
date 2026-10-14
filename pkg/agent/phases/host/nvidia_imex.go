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

	if err := installNVIDIAIMEXPackages(ctx, p.log, pm, nvidia.DriverVersion, driverMajor); err != nil {
		if !packageWasPresent {
			if cleanupErr := executil.RunCmd(ctx, p.log, executil.Systemctl(), "unmask", "nvidia-imex.service"); cleanupErr != nil {
				p.log.Warn("failed to remove temporary NVIDIA IMEX service mask", "error", cleanupErr)
			}
		}

		return err
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

func installNVIDIAIMEXPackages(
	ctx context.Context,
	log *slog.Logger,
	pm *hostPackageManager,
	driverVersion string,
	driverMajor int,
) error {
	if len(pm.refreshArgs) > 0 {
		if err := executil.RunCmd(ctx, log, pm.command, pm.refreshArgs...); err != nil {
			return fmt.Errorf("%s %s: %w", pm.name, strings.Join(pm.refreshArgs, " "), err)
		}
	}

	imexSpec, err := resolveNVIDIAIMEXPackage(ctx, log, driverVersion, driverMajor)
	if err != nil {
		return err
	}

	modprobeSpec, err := resolveMatchingDebianPackage(ctx, log, "nvidia-modprobe", driverVersion)
	if err != nil {
		return fmt.Errorf("resolve branch-matched NVIDIA modprobe package: %w", err)
	}

	specs := []string{imexSpec, modprobeSpec}
	missing := make([]string, 0, len(specs))

	for _, spec := range specs {
		name, version, _ := strings.Cut(spec, "=")

		installed, err := executil.OutputCmdAt(
			ctx,
			log,
			slog.LevelDebug,
			"dpkg-query",
			"--show",
			"--showformat=${Version}",
			name,
		)
		if err != nil || strings.TrimSpace(string(installed)) != version {
			missing = append(missing, spec)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	args := make([]string, 0, len(pm.installArgs)+len(missing))
	args = append(args, pm.installArgs...)
	args = append(args, missing...)

	if err := executil.RunCmd(ctx, log, pm.command, args...); err != nil {
		return fmt.Errorf("install branch-matched NVIDIA IMEX packages %s: %w", strings.Join(missing, " "), err)
	}

	return nil
}

func resolveNVIDIAIMEXPackage(
	ctx context.Context,
	log *slog.Logger,
	driverVersion string,
	driverMajor int,
) (string, error) {
	return resolveNVIDIAIMEXPackageWith(driverVersion, driverMajor, func(name string) (string, error) {
		return resolveMatchingDebianPackage(ctx, log, name, driverVersion)
	})
}

func resolveNVIDIAIMEXPackageWith(
	driverVersion string,
	driverMajor int,
	resolve func(string) (string, error),
) (string, error) {
	spec, err := resolve("nvidia-imex")
	if err == nil {
		return spec, nil
	}

	branchPackage := fmt.Sprintf("nvidia-imex-%d", driverMajor)

	spec, branchErr := resolve(branchPackage)
	if branchErr == nil {
		return spec, nil
	}

	return "", fmt.Errorf(
		"resolve branch-matched NVIDIA IMEX package for driver %s: unversioned package: %v; branch package %s: %w",
		driverVersion,
		err,
		branchPackage,
		branchErr,
	)
}

func resolveMatchingDebianPackage(
	ctx context.Context,
	log *slog.Logger,
	name string,
	driverVersion string,
) (string, error) {
	output, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "apt-cache", "madison", name)
	if err != nil {
		return "", fmt.Errorf("query apt versions for %s: %w", name, err)
	}

	version, err := parseMatchingDebianPackageVersion(string(output), driverVersion)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}

	return name + "=" + version, nil
}

func parseMatchingDebianPackageVersion(output, driverVersion string) (string, error) {
	prefix := strings.TrimSpace(driverVersion) + "-"

	for line := range strings.Lines(output) {
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}

		version := strings.TrimSpace(fields[1])
		if strings.HasPrefix(version, prefix) {
			return version, nil
		}
	}

	return "", fmt.Errorf("no package version matches active NVIDIA driver %s", driverVersion)
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
