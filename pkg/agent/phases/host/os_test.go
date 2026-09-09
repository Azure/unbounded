// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectHostPackageManagerUsesAPT(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManager(existingPathLookup("apt-get", "tdnf"))
	require.NoError(t, err)

	require.Equal(t, "apt-get", pm.name)
	require.Equal(t, debianRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"update", "-y"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y", "--no-install-recommends"}, pm.installArgs)
}

func TestDetectHostPackageManagerUsesTdnf(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManager(existingPathLookup("tdnf"))
	require.NoError(t, err)

	require.Equal(t, "tdnf", pm.name)
	require.Equal(t, rpmRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"makecache"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y"}, pm.installArgs)
}

func TestDetectHostPackageManagerUsesDnf(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManager(existingPathLookup("dnf"))
	require.NoError(t, err)

	require.Equal(t, "dnf", pm.name)
	require.Equal(t, rpmRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"makecache"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y"}, pm.installArgs)
}

// TestDetectHostPackageManagerRejectsUnsupportedHost covers a host with neither
// a package manager nor the tools the required packages provide. Such a host
// cannot be remediated, so detection must fail.
func TestDetectHostPackageManagerRejectsUnsupportedHost(t *testing.T) {
	t.Parallel()

	_, err := detectHostPackageManager(missingPathLookup)
	require.ErrorContains(t, err, "no supported package manager")
	// The error names what is missing, so an operator can supply it.
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

// lookupOnly returns a LookPath stub that resolves exactly the given binaries.
func lookupOnly(available ...string) func(string) (string, error) {
	set := make(map[string]struct{}, len(available))
	for _, name := range available {
		set[name] = struct{}{}
	}

	return func(name string) (string, error) {
		if _, ok := set[name]; ok {
			return "/usr/bin/" + name, nil
		}

		return "", errors.New("not found")
	}
}

// TestDetectHostPackageManagerCapabilityOnlyHost covers immutable hosts such as
// Azure Container Linux, which have no package manager but do ship the tools
// the required packages exist to provide.
func TestDetectHostPackageManagerCapabilityOnlyHost(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManager(lookupOnly("systemd-nspawn", "curl", "nft", "mountpoint"))
	require.NoError(t, err)
	require.Equal(t, "none", pm.name)
	require.Nil(t, pm.command)

	for _, pkg := range pm.requiredPackages {
		require.True(t, pm.installed(context.Background(), discardLogger(), pkg),
			"expected %s to be satisfied by its capability", pkg)
	}
}

// TestDetectHostPackageManagerCapabilityOnlyHostMissingTool verifies the error
// names both the package and the tool, so the operator knows what to supply.
func TestDetectHostPackageManagerCapabilityOnlyHostMissingTool(t *testing.T) {
	t.Parallel()

	// The tool a system extension would supply is the one that is missing.
	_, err := detectHostPackageManager(lookupOnly("curl", "nft", "mountpoint"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "systemd-container")
	require.Contains(t, err.Error(), "systemd-nspawn")
	require.Contains(t, err.Error(), "no supported package manager")
}

// TestDetectHostPackageManagerPrefersRealManagers ensures the capability path is
// a last resort and never shadows an installable host.
func TestDetectHostPackageManagerPrefersRealManagers(t *testing.T) {
	t.Parallel()

	for _, manager := range []string{"apt-get", "tdnf", "dnf"} {
		pm, err := detectHostPackageManager(lookupOnly(manager, "systemd-nspawn", "curl", "nft", "mountpoint"))
		require.NoError(t, err)
		require.Equal(t, manager, pm.name)
		require.NotNil(t, pm.command)
	}
}

// TestRPMPackageInstalledFallsBackToCapability covers Azure Container Linux,
// which ships tdnf and a populated rpm database but no rpm binary. Querying rpm
// there reports every package missing, which would send bootstrap to the
// network to install packages that are already present on a read-only /usr.
func TestRPMPackageInstalledFallsBackToCapability(t *testing.T) {
	t.Parallel()

	// tdnf is present, rpm is not, and every capability resolves.
	lookup := lookupOnly("tdnf", "systemd-nspawn", "curl", "nft", "mountpoint")

	pm, err := detectHostPackageManager(lookup)
	require.NoError(t, err)
	require.Equal(t, "tdnf", pm.name)

	for _, pkg := range pm.requiredPackages {
		require.True(t, pm.installed(context.Background(), discardLogger(), pkg),
			"expected %s to be reported installed via its capability", pkg)
	}
}

// TestRPMPackageInstalledReportsMissingCapability keeps the fallback honest: a
// package whose capability is absent must still be reported missing, so it can
// be installed rather than silently skipped.
func TestRPMPackageInstalledReportsMissingCapability(t *testing.T) {
	t.Parallel()

	// systemd-nspawn is the capability a system extension would supply.
	lookup := lookupOnly("tdnf", "curl", "nft", "mountpoint")

	pm, err := detectHostPackageManager(lookup)
	require.NoError(t, err)
	require.Equal(t, "tdnf", pm.name)

	require.False(t, pm.installed(context.Background(), discardLogger(), "systemd-container"))
	require.True(t, pm.installed(context.Background(), discardLogger(), "curl"))
}

// TestImageManagedHostNeverSelectsPackageInstallation covers Azure Container
// Linux, which ships tdnf but whose /usr is a read-only dm-verity image.
//
// Selecting tdnf there sends bootstrap to the network to install packages into
// a filesystem that cannot accept them. Presence of a package manager binary is
// not evidence that package mutation is supported.
func TestImageManagedHostNeverSelectsPackageInstallation(t *testing.T) {
	t.Parallel()

	// tdnf is present, and so is every required capability.
	lookup := lookupOnly("tdnf", "dnf", "systemd-nspawn", "curl", "nft", "mountpoint")

	pm, err := detectHostPackageManagerFor(lookup, true)
	require.NoError(t, err)

	require.Equal(t, "none", pm.name)
	require.Nil(t, pm.command, "an image-managed host must have nothing to install with")
}

// TestImageManagedHostReportsMissingPrerequisite pins the error an operator
// sees: a missing tool on an image-managed host is a prerequisite of the image,
// so the message must not suggest installing it.
func TestImageManagedHostReportsMissingPrerequisite(t *testing.T) {
	t.Parallel()

	lookup := lookupOnly("tdnf", "curl", "nft", "mountpoint")

	_, err := detectHostPackageManagerFor(lookup, true)
	require.Error(t, err)

	require.Contains(t, err.Error(), "image-managed")
	require.Contains(t, err.Error(), "systemd-container (provides systemd-nspawn)")
	require.NotContains(t, err.Error(), "no supported package manager",
		"tdnf is present, so blaming a missing package manager would be wrong")
}

// TestMutableHostStillInstalls keeps remediation working where it is supported,
// so the image-managed policy cannot regress ordinary RPM and Debian hosts.
func TestMutableHostStillInstalls(t *testing.T) {
	t.Parallel()

	for _, manager := range []string{"apt-get", "tdnf", "dnf"} {
		pm, err := detectHostPackageManagerFor(lookupOnly(manager), false)
		require.NoError(t, err)

		require.Equal(t, manager, pm.name)
		require.NotNil(t, pm.command)
	}
}
