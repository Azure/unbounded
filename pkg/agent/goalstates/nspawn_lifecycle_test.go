// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNSpawnLifecycleStateValidation(t *testing.T) {
	t.Parallel()

	complete := NvidiaHost{
		GPUDevicePaths: []string{"/dev/nvidia0"},
		LibMappings:    []NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
		DriverVersion:  "580.1",
	}

	require.NoError(t, (&NSpawnLifecycleState{
		Version:        NSpawnLifecycleStateVersion,
		MachineName:    "kube1",
		NVIDIARequired: true,
		NVIDIA:         complete,
	}).Validate("kube1"))

	err := (&NSpawnLifecycleState{Version: NSpawnLifecycleStateVersion, MachineName: "kube1", NVIDIARequired: true}).Validate("kube1")
	require.ErrorContains(t, err, "incomplete")

	err = (&NSpawnLifecycleState{Version: NSpawnLifecycleStateVersion, MachineName: "kube1", NVIDIA: complete}).Validate("kube1")
	require.ErrorContains(t, err, "CPU-provisioned")

	err = (&NSpawnLifecycleState{
		Version:     NSpawnLifecycleStateVersion,
		MachineName: "kube1",
		NVIDIA:      NvidiaHost{GPUDevicePaths: []string{"/dev/nvidia0"}},
	}).Validate("kube1")
	require.ErrorContains(t, err, "CPU-provisioned")

	err = (&NSpawnLifecycleState{Version: NSpawnLifecycleStateVersion + 1, MachineName: "kube1"}).Validate("kube1")
	require.ErrorContains(t, err, "unsupported")
}

func TestResolveContainerdUsesProvisionedNVIDIACapability(t *testing.T) {
	t.Parallel()

	require.False(t, ResolveContainerdForNVIDIACapability("", false).NvidiaRuntime.Enabled)
	require.True(t, ResolveContainerdForNVIDIACapability("", true).NvidiaRuntime.Enabled)
}

func TestNSpawnLifecycleStatePath(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/etc/unbounded/agent/kube1-nspawn-lifecycle.json", NSpawnLifecycleStatePath("kube1"))
}
