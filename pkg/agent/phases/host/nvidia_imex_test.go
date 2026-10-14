// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestParseNVIDIADriverMajor(t *testing.T) {
	t.Parallel()

	major, err := parseNVIDIADriverMajor("580.178.04")
	require.NoError(t, err)
	require.Equal(t, 580, major)

	_, err = parseNVIDIADriverMajor("invalid")
	require.Error(t, err)
}

func TestParseMatchingDebianPackageVersion(t *testing.T) {
	t.Parallel()

	output := ` nvidia-imex | 615.71.09-2ubuntu1 | https://developer.download.nvidia.com
 nvidia-imex | 580.178.04-1ubuntu1 | https://developer.download.nvidia.com
 nvidia-imex | 580.173.02-1ubuntu1 | https://developer.download.nvidia.com
`

	version, err := parseMatchingDebianPackageVersion(output, "580.178.04")
	require.NoError(t, err)
	require.Equal(t, "580.178.04-1ubuntu1", version)

	_, err = parseMatchingDebianPackageVersion(output, "580.999.01")
	require.ErrorContains(t, err, "no package version matches")
}

func TestResolveNVIDIAIMEXPackage(t *testing.T) {
	t.Parallel()

	t.Run("prefers unversioned package", func(t *testing.T) {
		t.Parallel()

		var queried []string

		spec, err := resolveNVIDIAIMEXPackageWith("580.178.04", 580, func(name string) (string, error) {
			queried = append(queried, name)

			return name + "=580.178.04-1ubuntu1", nil
		})
		require.NoError(t, err)
		require.Equal(t, "nvidia-imex=580.178.04-1ubuntu1", spec)
		require.Equal(t, []string{"nvidia-imex"}, queried)
	})

	t.Run("falls back to branch package", func(t *testing.T) {
		t.Parallel()

		var queried []string

		spec, err := resolveNVIDIAIMEXPackageWith("580.178.04", 580, func(name string) (string, error) {
			queried = append(queried, name)
			if name == "nvidia-imex" {
				return "", errors.New("not found")
			}

			return name + "=580.178.04-1ubuntu1", nil
		})
		require.NoError(t, err)
		require.Equal(t, "nvidia-imex-580=580.178.04-1ubuntu1", spec)
		require.Equal(t, []string{"nvidia-imex", "nvidia-imex-580"}, queried)
	})

	t.Run("fails when neither package matches", func(t *testing.T) {
		t.Parallel()

		_, err := resolveNVIDIAIMEXPackageWith("580.178.04", 580, func(name string) (string, error) {
			return "", errors.New("not found")
		})
		require.ErrorContains(t, err, "unversioned package")
		require.ErrorContains(t, err, "branch package nvidia-imex-580")
	})
}

func TestValidateNVIDIAIMEXPreinstallHost(t *testing.T) {
	t.Parallel()

	valid := goalstates.NvidiaHost{
		GPUDevicePaths:           []string{"/dev/nvidia0", "/dev/nvidia-caps"},
		LibMappings:              []goalstates.NvidiaLibMapping{{HostPath: "/usr/lib/libcuda.so"}},
		DriverVersion:            "580.178.04",
		IMEXManagementCapability: true,
		IMEXDeviceClass:          true,
	}

	major, err := validateNVIDIAIMEXPreinstallHost(valid)
	require.NoError(t, err)
	require.Equal(t, 580, major)

	missingCapability := valid
	missingCapability.IMEXManagementCapability = false
	_, err = validateNVIDIAIMEXPreinstallHost(missingCapability)
	require.ErrorContains(t, err, "fabric-imex-mgmt")
}

func TestValidateNVIDIAIMEXHostOS(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(path, []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\n"), 0o600))
	require.NoError(t, validateNVIDIAIMEXHostOS(path))

	require.NoError(t, os.WriteFile(path, []byte("ID=azurelinux\nVERSION_ID=3.0\n"), 0o600))
	require.ErrorContains(t, validateNVIDIAIMEXHostOS(path), "supports Ubuntu 24.04 only")
}
