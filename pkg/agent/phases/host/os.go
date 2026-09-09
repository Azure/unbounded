// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// debianRequiredPackages lists the OS packages that must be installed on a Debian host.
// - systemd-container: provides systemd-nspawn for running containers.
// - curl: used for downloading resources.
// - nftables: provides nft, used by nftables-flush.service and LocalDNS rules.
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

	// A capability-only host has nothing to install with. Detection already
	// refuses when a capability is absent, so reaching here means one
	// disappeared in between; fail explicitly rather than dereferencing a nil
	// command.
	if pm.command == nil {
		return fmt.Errorf(
			"host has no package manager and is missing required tools: %s",
			strings.Join(missing, ", "),
		)
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
	return detectHostPackageManagerFor(lookupPath, goalstates.HostIsImageManaged())
}

// detectHostPackageManagerFor selects how required tools are satisfied on a
// host whose OS content is, or is not, image-managed.
//
// The distinction is not cosmetic. Azure Container Linux ships tdnf, so keying
// only on "is there a package manager binary" selects package installation and
// then attempts it against a read-only dm-verity /usr. The image is the unit of
// delivery there, so a missing tool is a prerequisite to report rather than
// something to remediate, and the presence of tdnf is not evidence that package
// mutation is supported.
func detectHostPackageManagerFor(
	lookupPath func(string) (string, error),
	imageManaged bool,
) (*hostPackageManager, error) {
	if imageManaged {
		return imageManagedPackageManager(lookupPath)
	}

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
			installed:        rpmPackageInstalled(lookupPath),
		}, nil
	}

	if _, err := lookupPath("dnf"); err == nil {
		return &hostPackageManager{
			name:             "dnf",
			requiredPackages: rpmRequiredPackages,
			command:          executil.Dnf(),
			refreshArgs:      []string{"makecache"},
			installArgs:      []string{"install", "-y"},
			installed:        rpmPackageInstalled(lookupPath),
		}, nil
	}

	// Some hosts have no package manager at all. Immutable images such as Azure
	// Container Linux mount /usr read-only and ship no tdnf, dnf, rpm or
	// rpm-ostree, so there is nothing to install with and nothing to install
	// into. Such a host is still usable when the capabilities the required
	// packages exist to provide are already present, whether baked into the
	// image or supplied by a system extension.
	//
	// This is deliberately keyed on the capability rather than on the
	// distribution: what matters is whether the tools resolve, not which OS is
	// reporting.
	return capabilityOnlyPackageManager(lookupPath)
}

// packageCapabilities maps each required package to the executable it exists to
// provide. Probing the executable is what lets a host with no package manager
// satisfy the requirement.
var packageCapabilities = map[string]string{
	"systemd-container": "systemd-nspawn",
	"curl":              "curl",
	"nftables":          "nft",
	"util-linux":        "mountpoint",
}

// capabilitySatisfied reports whether the executable a required package exists
// to provide already resolves on PATH.
func capabilitySatisfied(lookupPath func(string) (string, error), pkg string) bool {
	binary, ok := packageCapabilities[pkg]
	if !ok {
		return false
	}

	_, err := lookupPath(binary)

	return err == nil
}

// capabilityOnlyPackageManager returns a package manager for hosts that cannot
// install anything, succeeding only when every required capability is already
// present.
func capabilityOnlyPackageManager(lookupPath func(string) (string, error)) (*hostPackageManager, error) {
	if missing := missingCapabilities(lookupPath); len(missing) > 0 {
		return nil, fmt.Errorf(
			"host has no supported package manager (apt-get, tdnf, or dnf) and is missing required tools: %s",
			strings.Join(missing, ", "),
		)
	}

	return capabilityManager(lookupPath), nil
}

// imageManagedPackageManager validates that an image-managed host already
// provides every required tool.
//
// The error deliberately does not mention installing anything: on this host
// there is nothing to install with and nowhere to install to, so the actionable
// remedy is a different image or a system extension.
func imageManagedPackageManager(lookupPath func(string) (string, error)) (*hostPackageManager, error) {
	if missing := missingCapabilities(lookupPath); len(missing) > 0 {
		return nil, fmt.Errorf(
			"host OS content is image-managed and its /usr is read-only, so these required tools "+
				"must be supplied by the image or a system extension rather than installed: %s",
			strings.Join(missing, ", "),
		)
	}

	return capabilityManager(lookupPath), nil
}

// missingCapabilities returns the required packages whose capability does not
// resolve, described so an operator knows what to supply.
func missingCapabilities(lookupPath func(string) (string, error)) []string {
	var missing []string

	for _, pkg := range rpmRequiredPackages {
		binary, ok := packageCapabilities[pkg]
		if !ok {
			missing = append(missing, pkg)

			continue
		}

		if _, err := lookupPath(binary); err != nil {
			missing = append(missing, fmt.Sprintf("%s (provides %s)", pkg, binary))
		}
	}

	return missing
}

// capabilityManager returns a package manager that can only report, never
// install.
func capabilityManager(lookupPath func(string) (string, error)) *hostPackageManager {
	return &hostPackageManager{
		name:             "none",
		requiredPackages: rpmRequiredPackages,
		installed: func(_ context.Context, _ *slog.Logger, pkg string) bool {
			return capabilitySatisfied(lookupPath, pkg)
		},
	}
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

// rpmPackageInstalled reports whether an RPM package is installed, preferring
// the rpm database and falling back to the capability the package provides.
//
// The fallback exists because an RPM host is not guaranteed to ship the rpm
// binary. Azure Container Linux has tdnf and a populated rpm database, but only
// rpm-libs: there is no /usr/bin/rpm, so `rpm -q` exits 127 and every required
// package looks missing. Bootstrap then reaches across the network to install
// packages that are already present, on a host whose /usr is a read-only
// dm-verity image and could not accept them anyway.
//
// Probing the capability instead answers the question the caller is really
// asking, which is whether the tool the package exists to provide is usable.
func rpmPackageInstalled(lookupPath func(string) (string, error)) func(context.Context, *slog.Logger, string) bool {
	return func(ctx context.Context, log *slog.Logger, pkg string) bool {
		if _, err := lookupPath("rpm"); err != nil {
			return capabilitySatisfied(lookupPath, pkg)
		}

		// rpm exits non-zero when the package is not installed, which is the
		// expected case here; log at debug so it is not shown as an error.
		_, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "rpm", "-q", "--quiet", pkg)

		return err == nil
	}
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
