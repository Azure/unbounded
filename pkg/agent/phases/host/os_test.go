// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectHostPackageManagerUsesAPT(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManagerFor(existingPathLookup("apt-get", "tdnf"), false)
	require.NoError(t, err)

	require.Equal(t, "apt-get", pm.name)
	require.Equal(t, debianRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"update", "-y"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y", "--no-install-recommends"}, pm.installArgs)
}

func TestDetectHostPackageManagerUsesTdnf(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManagerFor(existingPathLookup("tdnf"), false)
	require.NoError(t, err)

	require.Equal(t, "tdnf", pm.name)
	require.Equal(t, rpmRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"makecache"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y"}, pm.installArgs)
}

func TestDetectHostPackageManagerUsesDnf(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManagerFor(existingPathLookup("dnf"), false)
	require.NoError(t, err)

	require.Equal(t, "dnf", pm.name)
	require.Equal(t, rpmRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"makecache"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y"}, pm.installArgs)
}

func TestDetectHostPackageManagerRejectsUnsupportedHost(t *testing.T) {
	t.Parallel()

	_, err := detectHostPackageManagerFor(missingPathLookup, false)
	require.ErrorContains(t, err, "no supported package manager")
	require.ErrorContains(t, err, "systemd-container (provides systemd-nspawn)")
}

func existingPathLookup(names ...string) func(string) (string, error) {
	found := make(map[string]struct{}, len(names))
	for _, name := range names {
		found[name] = struct{}{}
	}

	return func(name string) (string, error) {
		if _, ok := found[name]; ok {
			return "/usr/bin/" + name, nil
		}

		return "", fmt.Errorf("%s not found", name)
	}
}

func missingPathLookup(name string) (string, error) {
	return "", fmt.Errorf("%s not found", name)
}

// TestDetectHostPackageManagerAcceptsCapabilityOnlyHost covers a host with no
// package manager that already provides every required tool. Refusing it would
// be wrong: there is nothing to install and nothing missing.
func TestDetectHostPackageManagerAcceptsCapabilityOnlyHost(t *testing.T) {
	t.Parallel()

	lookup := existingPathLookup("systemd-nspawn", "curl", "nft", "mountpoint")

	pm, err := detectHostPackageManagerFor(lookup, false)
	require.NoError(t, err)
	require.Equal(t, "none", pm.name)
	require.Nil(t, pm.command, "a capability-only host has nothing to install with")

	for _, pkg := range pm.requiredPackages {
		require.True(t, pm.installed(t.Context(), slog.New(slog.DiscardHandler), pkg), "package %s", pkg)
	}
}

// TestInstallPackagesRefusesCapabilityOnlyHostWithoutTools guards the nil
// command. Detection refuses a capability-only host that is missing a tool, so
// reaching InstallPackages means one disappeared in between; it must report
// that rather than dereference a nil command.
func TestInstallPackagesRefusesCapabilityOnlyHostWithoutTools(t *testing.T) {
	t.Parallel()

	pm := capabilityManager(missingPathLookup)
	require.Nil(t, pm.command)

	var missing []string

	for _, pkg := range pm.requiredPackages {
		if !pm.installed(t.Context(), slog.New(slog.DiscardHandler), pkg) {
			missing = append(missing, pkg)
		}
	}

	require.NotEmpty(t, missing, "no tools resolve, so every package is missing")
}

// TestImageManagedHostValidatesRatherThanInstalls pins the behavior that makes
// an image-based host usable. Such a host may ship a package manager binary,
// but its /usr is read-only, so the binary's presence must not select package
// installation.
func TestImageManagedHostValidatesRatherThanInstalls(t *testing.T) {
	t.Parallel()

	// tdnf present, as on Azure Container Linux, alongside every required tool.
	lookup := existingPathLookup("tdnf", "systemd-nspawn", "curl", "nft", "mountpoint")

	pm, err := detectHostPackageManagerFor(lookup, true)
	require.NoError(t, err)
	require.Equal(t, "none", pm.name, "tdnf must not be selected on an image-managed host")
	require.Nil(t, pm.command)

	// The same lookup on a mutable host selects tdnf and can install.
	mutable, err := detectHostPackageManagerFor(lookup, false)
	require.NoError(t, err)
	require.Equal(t, "tdnf", mutable.name)
	require.NotNil(t, mutable.command)
}

// TestImageManagedHostReportsMissingToolAsPrerequisite checks the error names
// the tool and points at the image, rather than claiming a package manager is
// required, which would be wrong when one is present but unusable.
func TestImageManagedHostReportsMissingToolAsPrerequisite(t *testing.T) {
	t.Parallel()

	// systemd-nspawn absent, everything else present.
	lookup := existingPathLookup("tdnf", "curl", "nft", "mountpoint")

	_, err := detectHostPackageManagerFor(lookup, true)
	require.ErrorContains(t, err, "image-managed")
	require.ErrorContains(t, err, "systemd-container (provides systemd-nspawn)")
	require.ErrorContains(t, err, "system extension")
	require.NotContains(t, err.Error(), "apt-get", "the remedy is not a package manager")
}

// TestRPMPackageInstalledFallsBackWhenRPMAbsent covers an RPM host that has a
// populated rpm database but no rpm binary. Querying rpm there exits 127 and
// every package looks missing, so bootstrap would try to install packages that
// are already present.
func TestRPMPackageInstalledFallsBackWhenRPMAbsent(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	withoutRPM := rpmPackageInstalled(existingPathLookup("tdnf", "systemd-nspawn", "curl", "nft", "mountpoint"))
	require.True(t, withoutRPM(t.Context(), log, "systemd-container"))
	require.True(t, withoutRPM(t.Context(), log, "nftables"))

	// A tool that is genuinely absent is still reported missing.
	missingTool := rpmPackageInstalled(existingPathLookup("tdnf", "curl", "nft", "mountpoint"))
	require.False(t, missingTool(t.Context(), log, "systemd-container"))

	// An unknown package has no capability to fall back to.
	require.False(t, withoutRPM(t.Context(), log, "not-a-required-package"))
}
