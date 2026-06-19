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

// requiredPackages lists the OS packages that must be installed on the host.
// - systemd-container: provides systemd-nspawn for running containers.
// - debootstrap: used to bootstrap a Debian rootfs.
// - curl: used for downloading resources.
// - nftables: provides nft, used by nftables-flush.service to reset firewall rules.
// - util-linux: provides mountpoint for private bpffs cleanup.
var requiredPackages = []string{
	"systemd-container",
	"debootstrap",
	"curl",
	"nftables",
	"util-linux",
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
	pm, err := detectPackageManager()
	if err != nil {
		return err
	}

	var missing []string

	for _, pkg := range requiredPackages {
		if !pm.isInstalled(ctx, ip.log, pkg) {
			missing = append(missing, pkg)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	if err := pm.install(ctx, ip.log, missing); err != nil {
		return fmt.Errorf("%s install %s: %w", pm.name, strings.Join(missing, " "), err)
	}

	return nil
}

type packageManager struct {
	name        string
	isInstalled func(context.Context, *slog.Logger, string) bool
	install     func(context.Context, *slog.Logger, []string) error
}

func detectPackageManager() (*packageManager, error) {
	return packageManagerForCommands(func(name string) bool {
		_, err := exec.LookPath(name)

		return err == nil
	})
}

func packageManagerForCommands(hasCommand func(string) bool) (*packageManager, error) {
	if hasCommand("apt-get") && hasCommand("dpkg-query") {
		return &packageManager{
			name:        "apt-get",
			isInstalled: isDebianPackageInstalled,
			install:     installDebianPackages,
		}, nil
	}

	if hasCommand("tdnf") && hasCommand("rpm") {
		return &packageManager{
			name:        "tdnf",
			isInstalled: isRPMPackageInstalled,
			install:     installTDNFPackages,
		}, nil
	}

	if hasCommand("dnf") && hasCommand("rpm") {
		return &packageManager{
			name:        "dnf",
			isInstalled: isRPMPackageInstalled,
			install:     installDNFPackages,
		}, nil
	}

	return nil, fmt.Errorf("no supported package manager found: need apt-get/dpkg-query, tdnf/rpm, or dnf/rpm")
}

func installDebianPackages(ctx context.Context, log *slog.Logger, missing []string) error {
	aptGet := executil.AptGet()

	if err := executil.RunCmd(ctx, log, aptGet, "update", "-y"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}

	args := append([]string{"install", "-y", "--no-install-recommends"}, missing...)
	if err := executil.RunCmd(ctx, log, aptGet, args...); err != nil {
		return fmt.Errorf("apt-get install: %w", err)
	}

	return nil
}

func installTDNFPackages(ctx context.Context, log *slog.Logger, missing []string) error {
	args := append([]string{"install", "-y", "--refresh"}, missing...)
	if err := executil.RunCmd(ctx, log, executil.TDNF(), args...); err != nil {
		return fmt.Errorf("tdnf install: %w", err)
	}

	return nil
}

func installDNFPackages(ctx context.Context, log *slog.Logger, missing []string) error {
	if err := executil.RunCmd(ctx, log, executil.DNF(), "makecache"); err != nil {
		return fmt.Errorf("dnf makecache: %w", err)
	}

	args := append([]string{"install", "-y", "--setopt=install_weak_deps=False"}, missing...)
	if err := executil.RunCmd(ctx, log, executil.DNF(), args...); err != nil {
		return fmt.Errorf("dnf install: %w", err)
	}

	return nil
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

func isRPMPackageInstalled(ctx context.Context, log *slog.Logger, pkg string) bool {
	_, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "rpm", "-q", pkg)

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
