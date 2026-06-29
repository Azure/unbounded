// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// debianRequiredPackages lists the OS packages that must be installed on a Debian host.
// - systemd-container: provides systemd-nspawn for running containers.
// - curl: used for downloading resources.
// - nftables: provides nft, used by nftables-flush.service to reset firewall rules.
// - util-linux: provides mountpoint for private bpffs cleanup.
var debianRequiredPackages = []string{
	"systemd-container",
	"curl",
	"nftables",
	"util-linux",
}

// rpmRequiredPackages lists the OS packages that must be installed on an RPM-based host.
var rpmRequiredPackages = []string{
	"systemd-container",
	"curl",
	"nftables",
	"util-linux",
}

type hostPackageManager struct {
	name             string
	requiredPackages []string
	command          func(context.Context) *exec.Cmd
	refreshArgs      []string
	installArgs      []string
	installed        func(context.Context, *slog.Logger, string) bool
}

type installPackages struct {
	log *slog.Logger
}

// InstallPackages returns a task that installs the required OS packages on the host.
func InstallPackages(log *slog.Logger) phases.Task {
	return &installPackages{log: log}
}

func (ip *installPackages) Name() string { return "install-packages" }

func (ip *installPackages) Do(ctx context.Context) error {
	pm, err := detectHostPackageManager(exec.LookPath)
	if err != nil {
		return err
	}

	var missing []string

	for _, pkg := range pm.requiredPackages {
		if !pm.installed(ctx, ip.log, pkg) {
			missing = append(missing, pkg)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	if len(pm.refreshArgs) > 0 {
		if err := executil.RunCmd(ctx, ip.log, pm.command, pm.refreshArgs...); err != nil {
			return fmt.Errorf("%s %s: %w", pm.name, strings.Join(pm.refreshArgs, " "), err)
		}
	}

	// Install all missing packages in a single invocation.
	args := make([]string, 0, len(pm.installArgs)+len(missing))
	args = append(args, pm.installArgs...)
	args = append(args, missing...)

	if err := executil.RunCmd(ctx, ip.log, pm.command, args...); err != nil {
		return fmt.Errorf("%s install %s: %w", pm.name, strings.Join(missing, " "), err)
	}

	return nil
}

func detectHostPackageManager(lookupPath func(string) (string, error)) (*hostPackageManager, error) {
	if _, err := lookupPath("apt-get"); err == nil {
		return &hostPackageManager{
			name:             "apt-get",
			requiredPackages: debianRequiredPackages,
			command:          executil.AptGet(),
			refreshArgs:      []string{"update", "-y"},
			installArgs:      []string{"install", "-y", "--no-install-recommends"},
			installed:        isDebianPackageInstalled,
		}, nil
	}

	if _, err := lookupPath("tdnf"); err == nil {
		return &hostPackageManager{
			name:             "tdnf",
			requiredPackages: rpmRequiredPackages,
			command:          executil.Tdnf(),
			refreshArgs:      []string{"makecache"},
			installArgs:      []string{"install", "-y"},
			installed:        isRPMPackageInstalled,
		}, nil
	}

	if _, err := lookupPath("dnf"); err == nil {
		return &hostPackageManager{
			name:             "dnf",
			requiredPackages: rpmRequiredPackages,
			command:          executil.Dnf(),
			refreshArgs:      []string{"makecache"},
			installArgs:      []string{"install", "-y"},
			installed:        isRPMPackageInstalled,
		}, nil
	}

	return nil, fmt.Errorf("unsupported host package manager: apt-get, tdnf, or dnf is required")
}

// isDebianPackageInstalled checks whether a package is fully installed using dpkg-query.
func isDebianPackageInstalled(ctx context.Context, log *slog.Logger, pkg string) bool {
	// dpkg-query exits non-zero and writes to stderr when the package is not
	// found; this is the expected case when the package needs to be installed.
	// Use Debug level so the "no packages found" message is not shown as an error.
	output, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "dpkg-query", "--show", "--showformat=${db:Status-Status}", pkg)
	if err != nil {
		return false
	}

	return strings.TrimSpace(output) == "installed"
}

// isRPMPackageInstalled checks whether an RPM package is installed.
func isRPMPackageInstalled(ctx context.Context, log *slog.Logger, pkg string) bool {
	_, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "rpm", "-q", "--quiet", pkg)

	return err == nil
}

// Kubernetes sysctl settings. Inside systemd-nspawn, /proc/sys is a read-only
// bind mount of the host's /proc/sys, so these must be applied on the host
// before kubelet starts. kubelet's ContainerManager (with
// --protect-kernel-defaults=true) verifies the expected values on startup and
// refuses to start if they are incorrect.
//
//go:embed assets/99-kubernetes-sysctl.conf
var kubernetesSysctlConfig []byte

const hostSysctlPath = "/etc/sysctl.d/99-kubernetes.conf"

type configureOS struct {
	log *slog.Logger
}

// ConfigureOS returns a task that writes host-level OS configuration (e.g. sysctl tunables)
// that must be in place before any nspawn machine starts so that kubelet inside the
// container sees the correct kernel parameter values.
func ConfigureOS(log *slog.Logger) phases.Task {
	return &configureOS{log: log}
}

func (c *configureOS) Name() string { return "configure-os" }

func (c *configureOS) Do(ctx context.Context) error {
	if err := utilio.WriteFile(hostSysctlPath, kubernetesSysctlConfig, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", hostSysctlPath, err)
	}

	if err := executil.RunCmd(ctx, c.log, executil.Sysctl(), "--system"); err != nil {
		return fmt.Errorf("sysctl --system: %w", err)
	}

	return nil
}
