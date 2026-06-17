// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
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
	require.Contains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"update", "-y"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y", "--no-install-recommends"}, pm.installArgs)
}

func TestDetectHostPackageManagerUsesTdnf(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManager(existingPathLookup("tdnf"))
	require.NoError(t, err)

	require.Equal(t, "tdnf", pm.name)
	require.Equal(t, azureLinuxRequiredPackages, pm.requiredPackages)
	require.NotContains(t, pm.requiredPackages, "debootstrap")
	require.Equal(t, []string{"makecache"}, pm.refreshArgs)
	require.Equal(t, []string{"install", "-y"}, pm.installArgs)
}

func TestDetectHostPackageManagerRejectsUnsupportedHost(t *testing.T) {
	t.Parallel()

	_, err := detectHostPackageManager(missingPathLookup)
	require.ErrorContains(t, err, "unsupported host package manager")
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
