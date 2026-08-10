// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNSpawnLifecycleStateValidation(t *testing.T) {
	t.Parallel()

	complete := NvidiaHost{
		Required:       true,
		GPUDevicePaths: []string{"/dev/nvidia0"},
		LibMappings:    []NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
		DriverVersion:  "580.1",
	}

	require.NoError(t, (&NSpawnLifecycleState{
		Version:     NSpawnLifecycleStateVersion,
		MachineName: "kube1",
		NVIDIA:      complete,
	}).Validate("kube1"))

	err := (&NSpawnLifecycleState{
		Version:     NSpawnLifecycleStateVersion,
		MachineName: "kube1",
		NVIDIA:      NvidiaHost{Required: true},
	}).Validate("kube1")
	require.ErrorContains(t, err, "incomplete")

	unexpected := complete
	unexpected.Required = false
	err = (&NSpawnLifecycleState{Version: NSpawnLifecycleStateVersion, MachineName: "kube1", NVIDIA: unexpected}).Validate("kube1")
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

func TestLoadNSpawnLifecycleState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state := NSpawnLifecycleState{
		Version:     NSpawnLifecycleStateVersion,
		MachineName: "kube1",
		NVIDIA: NvidiaHost{
			Required:       true,
			GPUDevicePaths: []string{"/dev/nvidia0"},
			LibMappings:    []NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
			DriverVersion:  "580.1",
		},
	}
	data, err := json.Marshal(&state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	got, err := LoadNSpawnLifecycleState(path, "kube1")
	require.NoError(t, err)
	require.Equal(t, &state, got)

	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	_, err = LoadNSpawnLifecycleState(path, "kube1")
	require.ErrorContains(t, err, "decode nspawn lifecycle state")
}

func TestLoadOrInferNVIDIACapabilityLegacyMigration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "missing-state.json")
	legacyDropIn := filepath.Join(dir, "99-nvidia-runtime.toml")

	required, err := LoadOrInferNVIDIACapability(statePath, legacyDropIn, "kube1")
	require.NoError(t, err)
	require.False(t, required)

	require.NoError(t, os.WriteFile(legacyDropIn, []byte("managed"), 0o600))
	required, err = LoadOrInferNVIDIACapability(statePath, legacyDropIn, "kube1")
	require.NoError(t, err)
	require.True(t, required)
}

func TestLoadOrInferNVIDIACapabilityDoesNotReplaceCorruptState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	legacyDropIn := filepath.Join(dir, "99-nvidia-runtime.toml")

	require.NoError(t, os.WriteFile(statePath, []byte("{"), 0o600))
	require.NoError(t, os.WriteFile(legacyDropIn, []byte("managed"), 0o600))

	_, err := LoadOrInferNVIDIACapability(statePath, legacyDropIn, "kube1")
	require.ErrorContains(t, err, "decode nspawn lifecycle state")

	state := NSpawnLifecycleState{Version: NSpawnLifecycleStateVersion, MachineName: "kube2"}
	data, err := json.Marshal(&state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, data, 0o600))

	_, err = LoadOrInferNVIDIACapability(statePath, legacyDropIn, "kube1")
	require.ErrorContains(t, err, "not \"kube1\"")
}

func TestResolveContainerdUsesProvisionedNVIDIACapability(t *testing.T) {
	t.Parallel()

	require.False(t, ResolveContainerd(ContainerdOptions{}).NvidiaRuntime.Enabled)
	require.True(t, ResolveContainerd(ContainerdOptions{NvidiaRequired: true}).NvidiaRuntime.Enabled)
}

func TestNSpawnLifecycleStatePath(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/etc/unbounded/agent/kube1-nspawn-lifecycle.json", NSpawnLifecycleStatePath("kube1"))
}
