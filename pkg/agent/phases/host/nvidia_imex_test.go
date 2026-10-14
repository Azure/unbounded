// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
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
